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

package amifamily

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/aws-sdk-go/service/ssm/ssmiface"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"

	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/pretty"
)

type AMIProvider struct {
	ssmCache               *cache.Cache
	ec2Cache               *cache.Cache
	kubernetesVersionCache *cache.Cache
	ssm                    ssmiface.SSMAPI
	kubeClient             client.Client
	ec2api                 ec2iface.EC2API
	cm                     *pretty.ChangeMonitor
	kubernetesInterface    kubernetes.Interface
}

type Image struct {
	*ec2.Image
	Requirements scheduling.Requirements
}

func NewImage(image *ec2.Image) *Image {
	return &Image{
		Image:        image,
		Requirements: requirementsFromImage(image),
	}
}

type MappingData struct {
	InstanceTypes       []*cloudprovider.InstanceType
	BlockDeviceMappings []*v1alpha1.BlockDeviceMapping
}

const (
	kubernetesVersionCacheKey = "kubernetesVersion"
)

func NewAMIProvider(kubeClient client.Client, kubernetesInterface kubernetes.Interface, ssm ssmiface.SSMAPI, ec2api ec2iface.EC2API,
	ssmCache, ec2Cache, kubernetesVersionCache *cache.Cache) *AMIProvider {
	return &AMIProvider{
		ssmCache:               ssmCache,
		ec2Cache:               ec2Cache,
		kubernetesVersionCache: kubernetesVersionCache,
		ssm:                    ssm,
		kubeClient:             kubeClient,
		ec2api:                 ec2api,
		cm:                     pretty.NewChangeMonitor(),
		kubernetesInterface:    kubernetesInterface,
	}
}

func (p *AMIProvider) KubeServerVersion(ctx context.Context) (string, error) {
	if version, ok := p.kubernetesVersionCache.Get(kubernetesVersionCacheKey); ok {
		return version.(string), nil
	}
	serverVersion, err := p.kubernetesInterface.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	version := fmt.Sprintf("%s.%s", serverVersion.Major, strings.TrimSuffix(serverVersion.Minor, "+"))
	p.kubernetesVersionCache.SetDefault(kubernetesVersionCacheKey, version)
	if p.cm.HasChanged("kubernetes-version", version) {
		logging.FromContext(ctx).With("kubernetes-version", version).Debugf("discovered kubernetes version")
	}
	return version, nil
}

// Get returns a set of AMIIDs and corresponding instance types. AMI may vary due to architecture, accelerator, etc
// If AMI overrides are specified in the AWSNodeTemplate, then only those AMIs will be chosen.
func (p *AMIProvider) Get(ctx context.Context, nodeTemplate *v1alpha1.AWSNodeTemplate, instanceTypes []*cloudprovider.InstanceType, amiFamily AMIFamily) (map[string]*MappingData, error) {
	kubernetesVersion, err := p.KubeServerVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting kubernetes version %w", err)
	}
	amiMapping := map[string]*MappingData{}
	amis, err := p.getImages(ctx, nodeTemplate)
	if err != nil {
		return nil, err
	}
	if len(amis) > 0 {
		sortAMIsByCreationDate(amis) // Iterate through AMIs in order of creation date to use the latest AMI
		for _, instanceType := range instanceTypes {
			for _, ami := range amis {
				if err = instanceType.Requirements.Compatible(ami.Requirements); err == nil {
					// Update AMI mapping with the added instance type
					if _, ok := amiMapping[aws.StringValue(ami.ImageId)]; ok {
						amiMapping[aws.StringValue(ami.ImageId)].InstanceTypes = append(amiMapping[aws.StringValue(ami.ImageId)].InstanceTypes, instanceType)
					} else {
						amiMapping[aws.StringValue(ami.ImageId)] = &MappingData{
							BlockDeviceMappings: lo.Map(ami.BlockDeviceMappings, func(m *ec2.BlockDeviceMapping, _ int) *v1alpha1.BlockDeviceMapping { return newBlockDeviceMapping(m) }),
							InstanceTypes:       []*cloudprovider.InstanceType{instanceType},
						}
					}
					break
				}
			}
		}
		if len(amiMapping) == 0 {
			return nil, fmt.Errorf("no instance types satisfy requirements of amis")
		}
	} else {
		for _, instanceType := range instanceTypes {
			amiID, err := p.defaultAMIFromSSM(ctx, amiFamily.SSMAlias(kubernetesVersion, instanceType))
			if err != nil {
				return nil, err
			}
			amis, err = p.selectImages(ctx, map[string]string{"aws-ids": amiID})
			if err != nil {
				return nil, fmt.Errorf("fetching images, %w", err)
			}
			if len(amis) == 0 {
				return nil, fmt.Errorf("expected one image, got %d", len(amis))
			}
			// Update AMI mapping with the added instance type
			if _, ok := amiMapping[aws.StringValue(amis[0].ImageId)]; ok {
				amiMapping[aws.StringValue(amis[0].ImageId)].InstanceTypes = append(amiMapping[aws.StringValue(amis[0].ImageId)].InstanceTypes, instanceType)
			} else {
				amiMapping[aws.StringValue(amis[0].ImageId)] = &MappingData{
					BlockDeviceMappings: lo.Map(amis[0].BlockDeviceMappings, func(m *ec2.BlockDeviceMapping, _ int) *v1alpha1.BlockDeviceMapping { return newBlockDeviceMapping(m) }),
					InstanceTypes:       []*cloudprovider.InstanceType{instanceType},
				}
			}
		}
	}
	return amiMapping, nil
}

func (p *AMIProvider) defaultAMIFromSSM(ctx context.Context, ssmQuery string) (string, error) {
	if id, ok := p.ssmCache.Get(ssmQuery); ok {
		return id.(string), nil
	}
	output, err := p.ssm.GetParameterWithContext(ctx, &ssm.GetParameterInput{Name: aws.String(ssmQuery)})
	if err != nil {
		return "", fmt.Errorf("getting ssm parameter %q, %w", ssmQuery, err)
	}
	ami := aws.StringValue(output.Parameter.Value)
	p.ssmCache.SetDefault(ssmQuery, ami)
	if p.cm.HasChanged("ssmquery-"+ssmQuery, ami) {
		logging.FromContext(ctx).With("ami", ami, "query", ssmQuery).Debugf("discovered new ami")
	}
	return ami, nil
}

func (p *AMIProvider) getImages(ctx context.Context, nodeTemplate *v1alpha1.AWSNodeTemplate) ([]*Image, error) {
	if len(nodeTemplate.Spec.AMISelector) == 0 {
		return nil, nil
	}
	return p.selectImages(ctx, nodeTemplate.Spec.AMISelector)
}

func (p *AMIProvider) selectImages(ctx context.Context, amiSelector map[string]string) ([]*Image, error) {
	ec2AMIs, err := p.fetchImagesFromEC2(ctx, amiSelector)
	if err != nil {
		return nil, err
	}
	if len(ec2AMIs) == 0 {
		return nil, fmt.Errorf("no amis exist given constraints")
	}
	return lo.Map(ec2AMIs, func(i *ec2.Image, _ int) *Image {
		return NewImage(i)
	}), nil
}

func (p *AMIProvider) fetchImagesFromEC2(ctx context.Context, amiSelector map[string]string) ([]*ec2.Image, error) {
	filters := getFilters(amiSelector)
	hash, err := hashstructure.Hash(filters, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	if err != nil {
		return nil, err
	}
	if amis, ok := p.ec2Cache.Get(fmt.Sprint(hash)); ok {
		return amis.([]*ec2.Image), nil
	}
	// This API is not paginated, so a single call suffices.
	output, err := p.ec2api.DescribeImagesWithContext(ctx, &ec2.DescribeImagesInput{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("describing images %+v, %w", filters, err)
	}

	p.ec2Cache.SetDefault(fmt.Sprint(hash), output.Images)
	amiIDs := lo.Map(output.Images, func(ami *ec2.Image, _ int) string { return *ami.ImageId })
	if p.cm.HasChanged("amiIDs", amiIDs) {
		logging.FromContext(ctx).With("ami-ids", amiIDs).Debugf("discovered images")
	}
	return output.Images, nil
}

func getFilters(amiSelector map[string]string) []*ec2.Filter {
	var filters []*ec2.Filter
	for key, value := range amiSelector {
		if key == "aws-ids" {
			filterValues := functional.SplitCommaSeparatedString(value)
			filters = append(filters, &ec2.Filter{
				Name:   aws.String("image-id"),
				Values: aws.StringSlice(filterValues),
			})
		} else {
			filters = append(filters, &ec2.Filter{
				Name:   aws.String(fmt.Sprintf("tag:%s", key)),
				Values: []*string{aws.String(value)},
			})
		}
	}
	return filters
}

func sortAMIsByCreationDate(amis []*Image) {
	sort.Slice(amis, func(i, j int) bool {
		itime, _ := time.Parse(time.RFC3339, aws.StringValue(amis[i].CreationDate))
		jtime, _ := time.Parse(time.RFC3339, aws.StringValue(amis[j].CreationDate))
		return itime.Unix() >= jtime.Unix()
	})
}

func requirementsFromImage(ec2Image *ec2.Image) scheduling.Requirements {
	requirements := scheduling.NewRequirements()
	for _, tag := range ec2Image.Tags {
		if v1alpha5.WellKnownLabels.Has(*tag.Key) {
			requirements.Add(scheduling.NewRequirement(*tag.Key, v1.NodeSelectorOpIn, *tag.Value))
		}
	}
	// Always add the architecture of an image as a requirement, irrespective of what's specified in EC2 tags.
	architecture := *ec2Image.Architecture
	if value, ok := v1alpha1.AWSToKubeArchitectures[architecture]; ok {
		architecture = value
	}
	requirements.Add(scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, architecture))
	return requirements
}

func newBlockDeviceMapping(in *ec2.BlockDeviceMapping) *v1alpha1.BlockDeviceMapping {
	ret := &v1alpha1.BlockDeviceMapping{
		DeviceName: in.DeviceName,
	}
	if in.Ebs != nil {
		ret.EBS = &v1alpha1.BlockDevice{
			DeleteOnTermination: in.Ebs.DeleteOnTermination,
			Encrypted:           in.Ebs.Encrypted,
			IOPS:                in.Ebs.Iops,
			KMSKeyID:            in.Ebs.KmsKeyId,
			SnapshotID:          in.Ebs.SnapshotId,
			Throughput:          in.Ebs.Throughput,
			VolumeSize:          lo.ToPtr(resource.MustParse(fmt.Sprintf("%dGi", aws.Int64Value(in.Ebs.VolumeSize)))),
			VolumeType:          lo.Ternary(aws.StringValue(in.Ebs.VolumeType) == "gp2", aws.String("gp3"), in.Ebs.VolumeType), // Default to GP3 if image is GP2
		}
	}
	return ret
}
