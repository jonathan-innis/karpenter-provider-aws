package integration_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"
	"github.com/aws/karpenter-core/pkg/utils/functional"
	"github.com/aws/karpenter-core/pkg/utils/resources"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	awstest "github.com/aws/karpenter/pkg/test"
)

var _ = Describe("Machine", func() {
	var nodeTemplate *v1alpha1.AWSNodeTemplate
	BeforeEach(func() {
		nodeTemplate = awstest.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{AWS: v1alpha1.AWS{
			SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": settings.FromContext(env.Context).ClusterName},
			SubnetSelector:        map[string]string{"karpenter.sh/discovery": settings.FromContext(env.Context).ClusterName},
		}})
	})
	// For standalone machines, there is no Provisioner owner, so we just list all machines and delete them all
	AfterEach(func() {
		env.CleanupObjects(functional.Pair[client.Object, client.ObjectList]{First: &v1alpha5.Machine{}, Second: &v1alpha5.MachineList{}})
	})
	It("should create a standard machine within the 'c' instance family", func() {
		machine := test.Machine(v1alpha5.Machine{
			Spec: v1alpha5.MachineSpec{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1alpha1.LabelInstanceCategory,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"c"},
					},
				},
				MachineTemplateRef: &v1alpha5.MachineTemplateRef{
					Name: nodeTemplate.Name,
				},
			},
		})
		env.ExpectCreated(nodeTemplate, machine)
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		machine = env.EventuallyExpectCreatedMachineCount("==", 1)[0]
		Expect(node.Labels).To(HaveKeyWithValue(v1alpha1.LabelInstanceCategory, "c"))
		Expect(machine.StatusConditions().IsHappy()).To(BeTrue())
	})
	It("should create a standard machine based on resource requests", func() {
		machine := test.Machine(v1alpha5.Machine{
			Spec: v1alpha5.MachineSpec{
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("3"),
						v1.ResourceMemory: resource.MustParse("64Gi"),
					},
				},
				MachineTemplateRef: &v1alpha5.MachineTemplateRef{
					Name: nodeTemplate.Name,
				},
			},
		})
		env.ExpectCreated(nodeTemplate, machine)
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		machine = env.EventuallyExpectCreatedMachineCount("==", 1)[0]
		Expect(resources.Fits(machine.Spec.Resources.Requests, node.Status.Allocatable))
		Expect(machine.StatusConditions().IsHappy()).To(BeTrue())
	})
	It("should create a machine propagating all the machine spec details", func() {
		machine := test.Machine(v1alpha5.Machine{
			Spec: v1alpha5.MachineSpec{
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "custom-value",
					},
					{
						Key:    "other-custom-taint",
						Effect: v1.TaintEffectNoExecute,
						Value:  "other-custom-value",
					},
				},
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("3"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
				Kubelet: &v1alpha5.KubeletConfiguration{
					ContainerRuntime: lo.ToPtr("containerd"),
					MaxPods:          lo.ToPtr[int32](110),
					PodsPerCore:      lo.ToPtr[int32](10),
					SystemReserved: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("200m"),
						v1.ResourceMemory:           resource.MustParse("200Mi"),
						v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					},
					KubeReserved: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("200m"),
						v1.ResourceMemory:           resource.MustParse("200Mi"),
						v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					},
					EvictionHard: map[string]string{
						"memory.available":   "5%",
						"nodefs.available":   "5%",
						"nodefs.inodesFree":  "5%",
						"imagefs.available":  "5%",
						"imagefs.inodesFree": "5%",
						"pid.available":      "3%",
					},
					EvictionSoft: map[string]string{
						"memory.available":   "10%",
						"nodefs.available":   "10%",
						"nodefs.inodesFree":  "10%",
						"imagefs.available":  "10%",
						"imagefs.inodesFree": "10%",
						"pid.available":      "6%",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available":   {Duration: time.Minute * 2},
						"nodefs.available":   {Duration: time.Minute * 2},
						"nodefs.inodesFree":  {Duration: time.Minute * 2},
						"imagefs.available":  {Duration: time.Minute * 2},
						"imagefs.inodesFree": {Duration: time.Minute * 2},
						"pid.available":      {Duration: time.Minute * 2},
					},
					EvictionMaxPodGracePeriod:   lo.ToPtr[int32](120),
					ImageGCHighThresholdPercent: lo.ToPtr[int32](85),
					ImageGCLowThresholdPercent:  lo.ToPtr[int32](80),
				},
				MachineTemplateRef: &v1alpha5.MachineTemplateRef{
					Name: nodeTemplate.Name,
				},
			},
		})
		env.ExpectCreated(nodeTemplate, machine)
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		machine = env.EventuallyExpectCreatedMachineCount("==", 1)[0]
		Expect(node.Spec.Taints).To(ContainElements(
			v1.Taint{
				Key:    "custom-taint",
				Effect: v1.TaintEffectNoSchedule,
				Value:  "custom-value",
			},
			v1.Taint{
				Key:    "other-custom-taint",
				Effect: v1.TaintEffectNoExecute,
				Value:  "other-custom-value",
			},
		))
		Expect(machine.StatusConditions().IsHappy()).To(BeTrue())
	})
})
