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

package scale_test

import (
	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	awstest "github.com/aws/karpenter/pkg/test"
	"github.com/aws/karpenter/test/pkg/debug"
)

var _ = Describe("Provisioning", Label(debug.NoWatch), Label(debug.NoEvents), func() {
	var provisioner *v1alpha5.Provisioner
	var nodeTemplate *v1alpha1.AWSNodeTemplate
	var deployment *appsv1.Deployment
	var selector labels.Selector
	var dsCount int

	BeforeEach(func() {
		nodeTemplate = awstest.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{AWS: v1alpha1.AWS{
			SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": settings.FromContext(env.Context).ClusterName},
			SubnetSelector:        map[string]string{"karpenter.sh/discovery": settings.FromContext(env.Context).ClusterName},
		}})
		provisioner = test.Provisioner(test.ProvisionerOptions{
			ProviderRef: &v1alpha5.MachineTemplateRef{
				Name: nodeTemplate.Name,
			},
			// No limits!!!
			// https://tenor.com/view/chaos-gif-22919457
			Limits: v1.ResourceList{},
		})
		deployment = test.Deployment(test.DeploymentOptions{
			PodOptions: test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("100m"),
						v1.ResourceMemory: resource.MustParse("100Mi"),
					},
				},
				TerminationGracePeriodSeconds: lo.ToPtr[int64](0),
			},
		})
		selector = labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels)
		// Zonal topology spread to avoid exhausting IPs in each subnet
		deployment.Spec.Template.Spec.TopologySpreadConstraints = []v1.TopologySpreadConstraint{
			{
				LabelSelector:     deployment.Spec.Selector,
				TopologyKey:       v1.LabelTopologyZone,
				MaxSkew:           1,
				WhenUnsatisfiable: v1.DoNotSchedule,
			},
		}
		// Get the DS pod count and use it to calculate the DS pod overhead
		dsCount = env.GetDaemonSetCount(provisioner)
	})
	It("should scale successfully on a node-dense scale-up", func() {
		replicas := 500
		expectedNodeCount := 500

		deployment.Spec.Replicas = lo.ToPtr[int32](int32(replicas))
		deployment.Spec.Template.Spec.Affinity = &v1.Affinity{
			PodAntiAffinity: &v1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
					{
						LabelSelector: deployment.Spec.Selector,
						TopologyKey:   v1.LabelHostname,
					},
				},
			},
		}

		By("waiting for the deployment to deploy all of its pods")
		env.ExpectCreated(deployment)
		env.EventuallyExpectPendingPodCount(selector, replicas)

		By("kicking off provisioning by applying the provisioner and nodeTemplate")
		env.ExpectCreated(provisioner, nodeTemplate)

		env.EventuallyExpectCreatedMachineCount("==", expectedNodeCount)
		env.EventuallyExpectCreatedNodeCount("==", expectedNodeCount)
		env.EventuallyExpectInitializedNodeCount("==", expectedNodeCount)
		env.EventuallyExpectHealthyPodCount(selector, replicas)
	})
	It("should scale successfully on a pod-dense scale-up", func() {
		replicas := 6600
		maxPodDensity := 110 + dsCount
		expectedNodeCount := 60

		deployment.Spec.Replicas = lo.ToPtr[int32](int32(replicas))
		provisioner.Spec.KubeletConfiguration = &v1alpha5.KubeletConfiguration{
			MaxPods: lo.ToPtr[int32](int32(maxPodDensity)),
		}
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1alpha1.LabelInstanceSize,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"4xlarge"},
			},
		}

		By("waiting for the deployment to deploy all of its pods")
		env.ExpectCreated(deployment)
		env.EventuallyExpectPendingPodCount(selector, replicas)

		By("kicking off provisioning by applying the provisioner and nodeTemplate")
		env.ExpectCreated(provisioner, nodeTemplate)

		env.EventuallyExpectCreatedMachineCount("==", expectedNodeCount)
		env.EventuallyExpectCreatedNodeCount("==", expectedNodeCount)
		env.EventuallyExpectInitializedNodeCount("==", expectedNodeCount)
		env.EventuallyExpectHealthyPodCount(selector, replicas)
	})
})
