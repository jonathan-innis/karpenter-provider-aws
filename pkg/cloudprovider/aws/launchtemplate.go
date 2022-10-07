/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/amifamily"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/pretty"
)

const (
	launchTemplateNameFormat  = "Karpenter-%s-%s"
	karpenterManagedTagKey    = "karpenter.k8s.aws/cluster"
	kubernetesVersionCacheKey = "kubernetesVersion"
)

type LaunchTemplateProvider struct {
	sync.Mutex
	ec2api                ec2iface.EC2API
	clientSet             *kubernetes.Clientset
	amiFamily             *amifamily.Resolver
	securityGroupProvider *SecurityGroupProvider
	cache                 *cache.Cache
	logger                *zap.SugaredLogger
	caBundle              *string
	cm                    *pretty.ChangeMonitor
}

func NewLaunchTemplateProvider(ctx context.Context, ec2api ec2iface.EC2API, clientSet *kubernetes.Clientset, amiFamily *amifamily.Resolver, securityGroupProvider *SecurityGroupProvider, caBundle *string, startAsync <-chan struct{}) *LaunchTemplateProvider {
	l := &LaunchTemplateProvider{
		ec2api:                ec2api,
		clientSet:             clientSet,
		logger:                logging.FromContext(ctx).Named("launchtemplate"),
		amiFamily:             amiFamily,
		securityGroupProvider: securityGroupProvider,
		cache:                 cache.New(CacheTTL, CacheCleanupInterval),
		caBundle:              caBundle,
		cm:                    pretty.NewChangeMonitor(),
	}
	l.cache.OnEvicted(l.onCacheEvicted)
	go func() {
		// only hydrate cache once elected leader
		select {
		case <-startAsync:
		case <-ctx.Done():
			return
		}
		l.hydrateCache(ctx)
	}()
	return l
}

func launchTemplateName(options *amifamily.LaunchTemplate) string {
	hash, err := hashstructure.Hash(options, hashstructure.FormatV2, nil)
	if err != nil {
		panic(fmt.Sprintf("hashing launch template, %s", err))
	}
	return fmt.Sprintf(launchTemplateNameFormat, options.ClusterName, fmt.Sprint(hash))
}

func (p *LaunchTemplateProvider) Get(ctx context.Context, provider *v1alpha1.AWS, nodeRequest *cloudprovider.NodeRequest, additionalLabels map[string]string) (map[string][]cloudprovider.InstanceType, error) {
	p.Lock()
	defer p.Unlock()
	// If Launch Template is directly specified then just use it
	if provider.LaunchTemplateName != nil {
		return map[string][]cloudprovider.InstanceType{ptr.StringValue(provider.LaunchTemplateName): nodeRequest.InstanceTypeOptions}, nil
	}
	instanceProfile, err := p.getInstanceProfile(ctx, provider)
	if err != nil {
		return nil, err
	}
	// Get constrained security groups
	securityGroupsIDs, err := p.securityGroupProvider.Get(ctx, provider)
	if err != nil {
		return nil, err
	}
	kubeServerVersion, err := p.kubeServerVersion(ctx)
	if err != nil {
		return nil, err
	}
	resolvedLaunchTemplates, err := p.amiFamily.Resolve(ctx, provider, nodeRequest, &amifamily.Options{
		ClusterName:             injection.GetOptions(ctx).ClusterName,
		ClusterEndpoint:         injection.GetOptions(ctx).ClusterEndpoint,
		AWSENILimitedPodDensity: injection.GetOptions(ctx).AWSENILimitedPodDensity,
		InstanceProfile:         instanceProfile,
		SecurityGroupsIDs:       securityGroupsIDs,
		Tags:                    provider.Tags,
		Labels:                  lo.Assign(nodeRequest.Template.Labels, additionalLabels),
		CABundle:                p.caBundle,
		KubernetesVersion:       kubeServerVersion,
	})
	if err != nil {
		return nil, err
	}
	launchTemplates := map[string][]cloudprovider.InstanceType{}
	for _, resolvedLaunchTemplate := range resolvedLaunchTemplates {
		// Ensure the launch template exists, or create it
		ec2LaunchTemplate, err := p.ensureLaunchTemplate(ctx, resolvedLaunchTemplate)
		if err != nil {
			return nil, err
		}
		launchTemplates[*ec2LaunchTemplate.LaunchTemplateName] = resolvedLaunchTemplate.InstanceTypes
	}
	return launchTemplates, nil
}

func (p *LaunchTemplateProvider) ensureLaunchTemplate(ctx context.Context, options *amifamily.LaunchTemplate) (*ec2.LaunchTemplate, error) {
	var launchTemplate *ec2.LaunchTemplate
	name := launchTemplateName(options)
	// Read from cache
	if launchTemplate, ok := p.cache.Get(name); ok {
		p.cache.SetDefault(name, launchTemplate)
		return launchTemplate.(*ec2.LaunchTemplate), nil
	}
	// Attempt to find an existing LT.
	output, err := p.ec2api.DescribeLaunchTemplatesWithContext(ctx, &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []*string{aws.String(name)},
	})
	// Create LT if one doesn't exist
	if IsNotFound(err) {
		launchTemplate, err = p.createLaunchTemplate(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("creating launch template, %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("describing launch templates, %w", err)
	} else if len(output.LaunchTemplates) != 1 {
		return nil, fmt.Errorf("expected to find one launch template, but found %d", len(output.LaunchTemplates))
	} else {
		if p.cm.HasChanged("launchtemplate-"+name, name) {
			logging.FromContext(ctx).Debugf("Discovered launch template %s", name)
		}
		launchTemplate = output.LaunchTemplates[0]
	}
	p.cache.SetDefault(name, launchTemplate)
	return launchTemplate, nil
}

func (p *LaunchTemplateProvider) createLaunchTemplate(ctx context.Context, options *amifamily.LaunchTemplate) (*ec2.LaunchTemplate, error) {
	userData, err := options.UserData.Script()
	if err != nil {
		return nil, err
	}
	output, err := p.ec2api.CreateLaunchTemplateWithContext(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(launchTemplateName(options)),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			BlockDeviceMappings: p.blockDeviceMappings(options.BlockDeviceMappings),
			IamInstanceProfile: &ec2.LaunchTemplateIamInstanceProfileSpecificationRequest{
				Name: aws.String(options.InstanceProfile),
			},
			SecurityGroupIds: aws.StringSlice(options.SecurityGroupsIDs),
			UserData:         aws.String(userData),
			ImageId:          aws.String(options.AMIID),
			MetadataOptions: &ec2.LaunchTemplateInstanceMetadataOptionsRequest{
				HttpEndpoint:            options.MetadataOptions.HTTPEndpoint,
				HttpProtocolIpv6:        options.MetadataOptions.HTTPProtocolIPv6,
				HttpPutResponseHopLimit: options.MetadataOptions.HTTPPutResponseHopLimit,
				HttpTokens:              options.MetadataOptions.HTTPTokens,
			},
			TagSpecifications: []*ec2.LaunchTemplateTagSpecificationRequest{
				{ResourceType: aws.String(ec2.ResourceTypeNetworkInterface), Tags: v1alpha1.MergeTags(ctx, options.Tags)},
			},
		},
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String(ec2.ResourceTypeLaunchTemplate),
				Tags:         v1alpha1.MergeTags(ctx, options.Tags, map[string]string{karpenterManagedTagKey: options.ClusterName}),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Debugf("Created launch template, %s", *output.LaunchTemplate.LaunchTemplateName)
	return output.LaunchTemplate, nil
}

func (p *LaunchTemplateProvider) blockDeviceMappings(blockDeviceMappings []*v1alpha1.BlockDeviceMapping) []*ec2.LaunchTemplateBlockDeviceMappingRequest {
	if len(blockDeviceMappings) == 0 {
		// The EC2 API fails with empty slices and expects nil.
		return nil
	}
	blockDeviceMappingsRequest := []*ec2.LaunchTemplateBlockDeviceMappingRequest{}
	for _, blockDeviceMapping := range blockDeviceMappings {
		blockDeviceMappingsRequest = append(blockDeviceMappingsRequest, &ec2.LaunchTemplateBlockDeviceMappingRequest{
			DeviceName: blockDeviceMapping.DeviceName,
			Ebs: &ec2.LaunchTemplateEbsBlockDeviceRequest{
				DeleteOnTermination: blockDeviceMapping.EBS.DeleteOnTermination,
				Encrypted:           blockDeviceMapping.EBS.Encrypted,
				VolumeType:          blockDeviceMapping.EBS.VolumeType,
				Iops:                blockDeviceMapping.EBS.IOPS,
				Throughput:          blockDeviceMapping.EBS.Throughput,
				KmsKeyId:            blockDeviceMapping.EBS.KMSKeyID,
				SnapshotId:          blockDeviceMapping.EBS.SnapshotID,
				VolumeSize:          p.volumeSize(blockDeviceMapping.EBS.VolumeSize),
			},
		})
	}
	return blockDeviceMappingsRequest
}

// volumeSize returns a GiB scaled value from a resource quantity or nil if the resource quantity passed in is nil
func (p *LaunchTemplateProvider) volumeSize(quantity *resource.Quantity) *int64 {
	if quantity == nil {
		return nil
	}
	return aws.Int64(int64(quantity.AsApproximateFloat64() / math.Pow(2, 30)))
}

// Invalidate deletes a launch template from cache if it exists
func (p *LaunchTemplateProvider) Invalidate(ctx context.Context, ltName string) {
	p.Lock()
	defer p.Unlock()
	defer p.cache.OnEvicted(p.onCacheEvicted)
	p.cache.OnEvicted(nil)
	logging.FromContext(ctx).Debugf("Invalidating launch template \"%s\" in the cache because it no longer exists", ltName)
	p.cache.Delete(ltName)
}

// hydrateCache queries for existing Launch Templates created by Karpenter for the current cluster and adds to the LT cache.
// Any error during hydration will result in a panic
func (p *LaunchTemplateProvider) hydrateCache(ctx context.Context) {
	clusterName := injection.GetOptions(ctx).ClusterName
	p.logger.Debugf("Hydrating the launch template cache with tags matching \"%s: %s\"", karpenterManagedTagKey, clusterName)
	if err := p.ec2api.DescribeLaunchTemplatesPagesWithContext(ctx, &ec2.DescribeLaunchTemplatesInput{
		Filters: []*ec2.Filter{{Name: aws.String(fmt.Sprintf("tag:%s", karpenterManagedTagKey)), Values: []*string{aws.String(clusterName)}}},
	}, func(output *ec2.DescribeLaunchTemplatesOutput, _ bool) bool {
		for _, lt := range output.LaunchTemplates {
			p.cache.SetDefault(*lt.LaunchTemplateName, lt)
		}
		return true
	}); err != nil {
		p.logger.Errorf(fmt.Sprintf("Unable to hydrate the AWS launch template cache, %s", err))
	}
	p.logger.Debugf("Finished hydrating the launch template cache with %d items", p.cache.ItemCount())
}

func (p *LaunchTemplateProvider) onCacheEvicted(key string, lt interface{}) {
	if key == kubernetesVersionCacheKey {
		return
	}
	p.Lock()
	defer p.Unlock()
	if _, expiration, _ := p.cache.GetWithExpiration(key); expiration.After(time.Now()) {
		return
	}
	launchTemplate := lt.(*ec2.LaunchTemplate)
	if _, err := p.ec2api.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{LaunchTemplateId: launchTemplate.LaunchTemplateId}); err != nil {
		p.logger.Errorf("Unable to delete launch template, %v", err)
		return
	}
	p.logger.Debugf("Deleted launch template %v (%v)", aws.StringValue(launchTemplate.LaunchTemplateName), aws.StringValue(launchTemplate.LaunchTemplateId))
}

func (p *LaunchTemplateProvider) getInstanceProfile(ctx context.Context, provider *v1alpha1.AWS) (string, error) {
	if provider.InstanceProfile != nil {
		return aws.StringValue(provider.InstanceProfile), nil
	}
	defaultProfile := injection.GetOptions(ctx).AWSDefaultInstanceProfile
	if defaultProfile == "" {
		return "", errors.New("neither spec.provider.instanceProfile nor --aws-default-instance-profile is specified")
	}
	return defaultProfile, nil
}

func (p *LaunchTemplateProvider) kubeServerVersion(ctx context.Context) (string, error) {
	if version, ok := p.cache.Get(kubernetesVersionCacheKey); ok {
		return version.(string), nil
	}
	serverVersion, err := p.clientSet.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	version := fmt.Sprintf("%s.%s", serverVersion.Major, strings.TrimSuffix(serverVersion.Minor, "+"))
	p.cache.SetDefault(kubernetesVersionCacheKey, version)
	if p.cm.HasChanged("kubernete-version", version) {
		logging.FromContext(ctx).Debugf("Discovered kubernetes version %s", version)
	}
	return version, nil
}
