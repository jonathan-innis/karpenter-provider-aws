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
	"net"
	"sort"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/ssm/ssmiface"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/cloudprovider/amifamily/bootstrap"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/utils/pretty"
)

func DefaultEBS(quantity resource.Quantity) *v1alpha1.BlockDevice {
	return &v1alpha1.BlockDevice{
		Encrypted:  aws.Bool(true),
		VolumeType: aws.String(ec2.VolumeTypeGp3),
		VolumeSize: lo.ToPtr(quantity),
	}
}

// Resolver is able to fill-in dynamic launch template parameters
type Resolver struct {
	amiProvider      *AMIProvider
	UserDataProvider *UserDataProvider
}

// Options define the static launch template parameters
type Options struct {
	ClusterName             string
	ClusterEndpoint         string
	AWSENILimitedPodDensity bool
	InstanceProfile         string
	CABundle                *string `hash:"ignore"`
	// Level-triggered fields that may change out of sync.
	KubernetesVersion string
	SecurityGroupsIDs []string
	Tags              map[string]string
	Labels            map[string]string `hash:"ignore"`
	KubeDNSIP         net.IP
}

// LaunchTemplate holds the dynamically generated launch template parameters
type LaunchTemplate struct {
	*Options
	UserData            bootstrap.Bootstrapper
	BlockDeviceMappings []*v1alpha1.BlockDeviceMapping
	MetadataOptions     *v1alpha1.MetadataOptions
	AMIID               string
	InstanceTypes       []*cloudprovider.InstanceType `hash:"ignore"`
}

func NewLaunchTemplate(options *Options, amiFamily AMIFamily, kubelet *v1alpha5.KubeletConfiguration, userDataString string,
	blockDeviceMappings []*v1alpha1.BlockDeviceMapping, metadataOptions *v1alpha1.MetadataOptions, amiID string, taints []v1.Taint,
	instanceTypes []*cloudprovider.InstanceType) *LaunchTemplate {

	return &LaunchTemplate{
		Options: options,
		UserData: amiFamily.UserData(
			kubelet,
			taints,
			options.Labels,
			options.CABundle,
			instanceTypes,
			aws.String(userDataString),
		),
		BlockDeviceMappings: blockDeviceMappings,
		MetadataOptions:     metadataOptions,
		AMIID:               amiID,
		InstanceTypes:       instanceTypes,
	}
}

// AMIFamily can be implemented to override the default logic for generating dynamic launch template parameters
type AMIFamily interface {
	UserData(kubeletConfig *v1alpha5.KubeletConfiguration, taints []v1.Taint, labels map[string]string, caBundle *string, instanceTypes []*cloudprovider.InstanceType, customUserData *string) bootstrap.Bootstrapper
	SSMAlias(version string, instanceType *cloudprovider.InstanceType) string
	DefaultBlockDeviceMappings(resource.Quantity) []*v1alpha1.BlockDeviceMapping
	DefaultMetadataOptions() *v1alpha1.MetadataOptions
	EphemeralBlockDevice() *string
	FeatureFlags() FeatureFlags
}

// FeatureFlags describes whether the features below are enabled for a given AMIFamily
type FeatureFlags struct {
	UsesENILimitedMemoryOverhead bool
	PodsPerCoreEnabled           bool
	EvictionSoftEnabled          bool
}

// DefaultFamily provides default values for AMIFamilies that compose it
type DefaultFamily struct{}

func (d DefaultFamily) FeatureFlags() FeatureFlags {
	return FeatureFlags{
		UsesENILimitedMemoryOverhead: true,
		PodsPerCoreEnabled:           true,
		EvictionSoftEnabled:          true,
	}
}

// New constructs a new launch template Resolver
func New(kubeClient client.Client, ssm ssmiface.SSMAPI, ec2api ec2iface.EC2API, ssmCache *cache.Cache, ec2Cache *cache.Cache) *Resolver {
	return &Resolver{
		amiProvider: &AMIProvider{
			ssm:        ssm,
			ssmCache:   ssmCache,
			ec2Cache:   ec2Cache,
			kubeClient: kubeClient,
			ec2api:     ec2api,
			cm:         pretty.NewChangeMonitor(),
		},
		UserDataProvider: NewUserDataProvider(kubeClient),
	}
}

// Resolve generates launch templates using the static options and dynamically generates launch template parameters.
// Multiple ResolvedTemplates are returned based on the instanceTypes passed in to support special AMIs for certain instance types like GPUs.
func (r Resolver) Resolve(ctx context.Context, provider *v1alpha1.AWS, nodeRequest *cloudprovider.NodeRequest, options *Options) ([]*LaunchTemplate, error) {
	userDataString, err := r.UserDataProvider.Get(ctx, nodeRequest.Template.ProviderRef)
	if err != nil {
		return nil, err
	}
	amiFamily := GetAMIFamily(provider.AMIFamily, options)
	amiIDs, err := r.amiProvider.Get(ctx, provider, nodeRequest, options, amiFamily)
	if err != nil {
		return nil, err
	}
	var resolvedTemplates []*LaunchTemplate
	for amiID, instanceTypes := range amiIDs {
		metadataOptions := lo.Ternary(provider.MetadataOptions != nil, provider.MetadataOptions, amiFamily.DefaultMetadataOptions())
		taints := append(nodeRequest.Template.Taints, nodeRequest.Template.StartupTaints...)

		// BlockDeviceMappings will be dynamically provisioned based on instance type buckets; otherwise, use the block
		// device mappings set by the user
		if provider.BlockDeviceMappings == nil {
			buckets := computeEphemeralStorageBuckets(nodeRequest, instanceTypes)
			for _, bucket := range buckets {
				resolvedTemplates = append(resolvedTemplates, NewLaunchTemplate(options, amiFamily, nodeRequest.Template.KubeletConfiguration, userDataString,
					amiFamily.DefaultBlockDeviceMappings(bucket.First), metadataOptions, amiID, taints, bucket.Second))
			}
		} else {
			resolvedTemplates = append(resolvedTemplates,
				NewLaunchTemplate(options, amiFamily, nodeRequest.Template.KubeletConfiguration, userDataString,
					provider.BlockDeviceMappings, metadataOptions, amiID, taints, instanceTypes),
			)
		}
	}
	return resolvedTemplates, nil
}

// computeEphemeralStorageBuckets partitions the ephemeral-requests from all instance types into buckets to reduce
// the number of launchTemplates we need to create due to the differing blockDeviceMappings
// This is a statically defined value right now
func computeEphemeralStorageBuckets(nodeRequest *cloudprovider.NodeRequest, instanceTypes []*cloudprovider.InstanceType) []functional.Pair[resource.Quantity, []*cloudprovider.InstanceType] {
	ephemeralStorageRequests := lo.Map(instanceTypes, func(it *cloudprovider.InstanceType, _ int) functional.Pair[resource.Quantity, *cloudprovider.InstanceType] {
		quantity := resource.MustParse("0")
		quantity.Add(it.Overhead.Total()[v1.ResourceEphemeralStorage])
		quantity.Add(nodeRequest.Template.Requests[v1.ResourceEphemeralStorage])
		return functional.Pair[resource.Quantity, *cloudprovider.InstanceType]{First: quantity, Second: it}
	})
	sort.Slice(ephemeralStorageRequests, func(i, j int) bool {
		return ephemeralStorageRequests[i].First.AsApproximateFloat64() > ephemeralStorageRequests[j].First.AsApproximateFloat64()
	})
	var buckets []functional.Pair[resource.Quantity, []*cloudprovider.InstanceType]
	currentBucket := 0
	for i, request := range ephemeralStorageRequests {
		if i == 0 {
			buckets = append(buckets, functional.Pair[resource.Quantity, []*cloudprovider.InstanceType]{
				First:  request.First,
				Second: []*cloudprovider.InstanceType{request.Second},
			})
			continue
		}
		copied := buckets[currentBucket].First.DeepCopy()
		copied.Sub(request.First)
		maxDelta := resource.MustParse("10Gi")
		if copied.AsApproximateFloat64() > maxDelta.AsApproximateFloat64() {
			buckets = append(buckets, functional.Pair[resource.Quantity, []*cloudprovider.InstanceType]{
				First:  request.First,
				Second: []*cloudprovider.InstanceType{request.Second},
			})
			currentBucket++
		} else {
			buckets[currentBucket].Second = append(buckets[currentBucket].Second, request.Second)
		}
	}
	return buckets
}

func GetAMIFamily(amiFamily *string, options *Options) AMIFamily {
	switch aws.StringValue(amiFamily) {
	case v1alpha1.AMIFamilyBottlerocket:
		return &Bottlerocket{Options: options}
	case v1alpha1.AMIFamilyUbuntu:
		return &Ubuntu{Options: options}
	case v1alpha1.AMIFamilyCustom:
		return &Custom{Options: options}
	default:
		return &AL2{Options: options}
	}
}

func (o Options) DefaultMetadataOptions() *v1alpha1.MetadataOptions {
	return &v1alpha1.MetadataOptions{
		HTTPEndpoint:            aws.String(ec2.LaunchTemplateInstanceMetadataEndpointStateEnabled),
		HTTPProtocolIPv6:        aws.String(lo.Ternary(o.KubeDNSIP == nil || o.KubeDNSIP.To4() != nil, ec2.LaunchTemplateInstanceMetadataProtocolIpv6Disabled, ec2.LaunchTemplateInstanceMetadataProtocolIpv6Enabled)),
		HTTPPutResponseHopLimit: aws.Int64(2),
		HTTPTokens:              aws.String(ec2.LaunchTemplateHttpTokensStateRequired),
	}
}
