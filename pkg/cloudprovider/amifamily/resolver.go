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
	"net"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/samber/lo"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/resources"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/cloudprovider/amifamily/bootstrap"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

const (
	nodeFSAvailableSignal = "nodefs.available"
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
	amiProvider *AMIProvider
}

// Options define the static launch template parameters
type Options struct {
	ClusterName             string
	ClusterEndpoint         string
	AWSENILimitedPodDensity bool
	InstanceProfile         string
	CABundle                *string `hash:"ignore"`
	// Level-triggered fields that may change out of sync.
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
	blockDeviceMappings []*v1alpha1.BlockDeviceMapping, metadataOptions *v1alpha1.MetadataOptions, amiID string, taints []core.Taint,
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
	SSMAlias(version string, instanceType *cloudprovider.InstanceType) string
	UserData(kubeletConfig *v1alpha5.KubeletConfiguration, taints []core.Taint, labels map[string]string, caBundle *string, instanceTypes []*cloudprovider.InstanceType, customUserData *string) bootstrap.Bootstrapper
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
func New(amiProvider *AMIProvider) *Resolver {
	return &Resolver{
		amiProvider: amiProvider,
	}
}

// Resolve generates launch templates using the static options and dynamically generates launch template parameters.
// Multiple ResolvedTemplates are returned based on the instanceTypes passed in to support special AMIs for certain instance types like GPUs.
func (r Resolver) Resolve(ctx context.Context, nodeTemplate *v1alpha1.AWSNodeTemplate, machine *v1alpha5.Machine, instanceTypes []*cloudprovider.InstanceType, options *Options) ([]*LaunchTemplate, error) {
	amiFamily := GetAMIFamily(nodeTemplate.Spec.AMIFamily, options)
	amiIDs, err := r.amiProvider.Get(ctx, nodeTemplate, instanceTypes, amiFamily)
	if err != nil {
		return nil, err
	}
	var templates []*LaunchTemplate
	for amiID, its := range amiIDs {
		metadataOptions := lo.Ternary(nodeTemplate.Spec.MetadataOptions != nil, nodeTemplate.Spec.MetadataOptions, amiFamily.DefaultMetadataOptions())
		taints := append(machine.Spec.Taints, machine.Spec.StartupTaints...)

		var blockDeviceMappings []*v1alpha1.BlockDeviceMapping
		minEphemeralStorage := resource.MustParse("10Gi")
		if len(nodeTemplate.Spec.BlockDeviceMappings) == 0 {
			blockDeviceMappings = amiFamily.DefaultBlockDeviceMappings(resources.Max(minEphemeralStorage, neededEphemeralStorage(machine.Spec.Resources.Requests[core.ResourceEphemeralStorage], machine.Spec.Kubelet)))
		} else {
			// Either provision the volume size to be what was requested or have it be what is needed to run all the resource requests
			blockDeviceMappings[0].EBS.VolumeSize = lo.ToPtr(resources.Max(minEphemeralStorage, *blockDeviceMappings[0].EBS.VolumeSize, neededEphemeralStorage(machine.Spec.Resources.Requests[core.ResourceEphemeralStorage], machine.Spec.Kubelet)))
		}
		templates = append(templates, NewLaunchTemplate(options, amiFamily, machine.Spec.Kubelet, lo.FromPtr(nodeTemplate.Spec.UserData),
			blockDeviceMappings, metadataOptions, amiID, taints, its))
	}
	return templates, nil
}

// neededEphemeralStorage calculates how much ephemeral storage will be needed by the node given the requested
// ephemeral storage and the KubeletConfiguration specified by the machine
func neededEphemeralStorage(quantity resource.Quantity, kc *v1alpha5.KubeletConfiguration) resource.Quantity {
	systemOverhead := resource.MustParse("2Gi") // TODO @joinnis: Use the kubeletConfiguration as an overlay here
	var evictionThreshold resource.Quantity
	if kc == nil || kc.EvictionHard == nil {
		// If not set, the default EvictionHard value for nodefs.available is 10%
		evictionThreshold = *resources.Quantity(fmt.Sprint(quantity.AsApproximateFloat64() / 0.9)) // We need to find x in (x * 0.9 = quantity)
	} else {
		// If EvictionHard is set, we need to increase our filesystem by the nodefs.available set value
		if v, ok := kc.EvictionHard[nodeFSAvailableSignal]; ok {
			if strings.HasSuffix(v, "%") {
				p := lo.Must(functional.ParsePercentage(v))
				if p == 100 {
					p = 0
				}
				// We need to find x in (x * (1 - p) = quantity)
				evictionThreshold = *resources.Quantity(fmt.Sprint(quantity.AsApproximateFloat64() / ((100 - p) / 100)))
			} else {
				evictionThreshold = resources.Sum(quantity, resource.MustParse(v))
			}
		}
	}
	// We only care about EvictionSoft threshold here if it is greater than the EvictionHard
	if kc != nil && kc.EvictionSoft != nil {
		if v, ok := kc.EvictionSoft[nodeFSAvailableSignal]; ok {
			if strings.HasSuffix(v, "%") {
				p := lo.Must(functional.ParsePercentage(v))
				if p == 100 {
					p = 0
				}
				// We need to find x in (x * (1 - p) = quantity)
				evictionThreshold = resources.Max(evictionThreshold, *resources.Quantity(fmt.Sprint(quantity.AsApproximateFloat64() / ((100 - p) / 100))))
			} else {
				evictionThreshold = resources.Max(evictionThreshold, resources.Sum(quantity, resource.MustParse(v)))
			}
		}
	}
	return resources.Sum(systemOverhead, evictionThreshold)
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
