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

package termination_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	clock "k8s.io/utils/clock/testing"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider/fake"
	"github.com/aws/karpenter/pkg/controllers/termination"
	"github.com/aws/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"

	. "github.com/aws/karpenter/pkg/test/expectations"
)

var ctx context.Context
var controller *termination.Controller
var evictionQueue *termination.EvictionQueue
var env *test.Environment
var defaultOwnerRefs = []metav1.OwnerReference{{Kind: "ReplicaSet", APIVersion: "appsv1", Name: "rs", UID: "1234567890"}}
var fakeClock *clock.FakeClock

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Termination")
}

var _ = BeforeSuite(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	env = test.NewEnvironment(ctx, func(e *test.Environment) {
		cloudProvider := &fake.CloudProvider{}
		coreV1Client := corev1.NewForConfigOrDie(e.Config)
		recorder := test.NewEventRecorder()
		evictionQueue = termination.NewEvictionQueue(ctx, coreV1Client, recorder)
		controller = &termination.Controller{
			KubeClient: e.Client,
			Terminator: &termination.Terminator{
				KubeClient:    e.Client,
				CoreV1Client:  coreV1Client,
				CloudProvider: cloudProvider,
				EvictionQueue: evictionQueue,
				Clock:         fakeClock,
			},
			Recorder:          recorder,
			TerminationRecord: sets.NewString(),
		}
	})
	Expect(env.Start()).To(Succeed(), "Failed to start environment")
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Termination", func() {
	var node *v1.Node

	BeforeEach(func() {
		node = test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1alpha5.TerminationFinalizer}}})
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
		fakeClock.SetTime(time.Now())
	})

	Context("Reconciliation", func() {
		It("should delete nodes", func() {
			ExpectApplied(ctx, env.Client, node)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should exclude nodes from load balancers when terminating", func() {
			// This is a kludge to prevent the node from being deleted before we can
			// inspect its labels
			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					Annotations:     map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
					OwnerReferences: defaultOwnerRefs,
				},
			})

			ExpectApplied(ctx, env.Client, node, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels[v1.LabelNodeExcludeBalancers]).Should(Equal("karpenter"))
		})
		It("should not evict pods that tolerate unschedulable taint", func() {
			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podSkip := test.Pod(test.PodOptions{
				NodeName:    node.Name,
				Tolerations: []v1.Toleration{{Key: v1.TaintNodeUnschedulable, Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule}},
				ObjectMeta:  metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
			})
			ExpectApplied(ctx, env.Client, node, podEvict, podSkip)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Expect podEvict to be evicting, and delete it
			ExpectEvicted(env.Client, podEvict)
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not delete nodes that have a do-not-evict pod", func() {
			podEvict := test.Pod(test.PodOptions{
				NodeName:   node.Name,
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
			})
			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					Annotations:     map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
					OwnerReferences: defaultOwnerRefs,
				},
			})

			ExpectApplied(ctx, env.Client, node, podEvict, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect no pod to be enqueued for eviction
			ExpectNotEnqueuedForEviction(evictionQueue, podEvict, podNoEvict)

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Delete do-not-evict pod
			ExpectDeleted(ctx, env.Client, podNoEvict)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect podEvict to be enqueued for eviction then be successful
			ExpectEvicted(env.Client, podEvict)

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not delete nodes that have a do-not-evict pod that tolerates an unschedulable taint", func() {
			podEvict := test.Pod(test.PodOptions{
				NodeName:   node.Name,
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
			})
			podNoEvict := test.Pod(test.PodOptions{
				NodeName:    node.Name,
				Tolerations: []v1.Toleration{{Key: v1.TaintNodeUnschedulable, Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule}},
				ObjectMeta: metav1.ObjectMeta{
					Annotations:     map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
					OwnerReferences: defaultOwnerRefs,
				},
			})

			ExpectApplied(ctx, env.Client, node, podEvict, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect no pod to be enqueued for eviction
			ExpectNotEnqueuedForEviction(evictionQueue, podEvict, podNoEvict)

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Delete do-not-evict pod
			ExpectDeleted(ctx, env.Client, podNoEvict)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect podEvict to be enqueued for eviction then be successful
			ExpectEvicted(env.Client, podEvict)

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not delete nodes that have a do-not-evict static pod", func() {
			ExpectApplied(ctx, env.Client, node)
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			podEvict := test.Pod(test.PodOptions{
				NodeName:   node.Name,
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
			})
			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
					}},
				},
			})

			ExpectApplied(ctx, env.Client, podEvict, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect no pod to be enqueued for eviction
			ExpectNotEnqueuedForEviction(evictionQueue, podEvict, podNoEvict)

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Delete do-not-evict pod
			ExpectDeleted(ctx, env.Client, podNoEvict)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect podEvict to be enqueued for eviction then be successful
			ExpectEvicted(env.Client, podEvict)

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not delete nodes that have pods without an owner ref", func() {
			podEvict := test.Pod(test.PodOptions{
				NodeName:   node.Name,
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
			})
			podNoEvict := test.Pod(test.PodOptions{NodeName: node.Name})

			ExpectApplied(ctx, env.Client, node, podEvict, podNoEvict)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect no pod to be enqueued for eviction
			ExpectNotEnqueuedForEviction(evictionQueue, podEvict, podNoEvict)

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Delete no owner refs pod
			ExpectDeleted(ctx, env.Client, podNoEvict)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect podEvict to be enqueued for eviction then be successful
			ExpectEvicted(env.Client, podEvict)

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should delete nodes with terminal pods", func() {
			podEvictPhaseSucceeded := test.Pod(test.PodOptions{
				NodeName: node.Name,
				Phase:    v1.PodSucceeded,
			})
			podEvictPhaseFailed := test.Pod(test.PodOptions{
				NodeName: node.Name,
				Phase:    v1.PodFailed,
			})

			ExpectApplied(ctx, env.Client, node, podEvictPhaseSucceeded, podEvictPhaseFailed)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			// Trigger Termination Controller, which should ignore these pods and delete the node
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should delete nodes that have do-not-evict on pods for which it does not apply", func() {
			pods := []*v1.Pod{
				test.Pod(test.PodOptions{
					NodeName: node.Name,
					ObjectMeta: metav1.ObjectMeta{
						Annotations:     map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
						OwnerReferences: defaultOwnerRefs,
					},
				}),
				test.Pod(test.PodOptions{
					NodeName:    node.Name,
					Tolerations: []v1.Toleration{{Operator: v1.TolerationOpExists}},
					ObjectMeta:  metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
				}),
				test.Pod(test.PodOptions{
					NodeName:   node.Name,
					ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
				}),
			}
			ExpectApplied(ctx, env.Client, node)
			for _, pod := range pods {
				ExpectApplied(ctx, env.Client, pod)
			}
			// Trigger eviction
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			for _, pod := range pods {
				Expect(env.Client.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.Int64(30)})).To(Succeed())
			}
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			// Simulate stuck terminating
			fakeClock.Step(2 * time.Minute)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should fail to evict pods that violate a PDB", func() {
			minAvailable := intstr.FromInt(1)
			labelSelector := map[string]string{test.RandomName(): test.RandomName()}
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels: labelSelector,
				// Don't let any pod evict
				MinAvailable: &minAvailable,
			})
			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					Labels:          labelSelector,
					OwnerReferences: defaultOwnerRefs,
				},
				Phase: v1.PodRunning,
			})

			ExpectApplied(ctx, env.Client, node, podNoEvict, pdb)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Expect podNoEvict to fail eviction due to PDB, and be retried
			Eventually(func() int {
				return evictionQueue.NumRequeues(client.ObjectKeyFromObject(podNoEvict))
			}).Should(BeNumerically(">=", 1))

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podNoEvict)
			ExpectNotFound(ctx, env.Client, podNoEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should evict non-critical pods first", func() {
			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podNodeCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-node-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podClusterCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-cluster-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})

			ExpectApplied(ctx, env.Client, node, podEvict, podNodeCritical, podClusterCritical)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Expect podEvict to be evicting, and delete it
			ExpectEvicted(env.Client, podEvict)
			ExpectDeleted(ctx, env.Client, podEvict)

			// Expect the critical pods to be evicted and deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectEvicted(env.Client, podNodeCritical)
			ExpectDeleted(ctx, env.Client, podNodeCritical)
			ExpectEvicted(env.Client, podClusterCritical)
			ExpectDeleted(ctx, env.Client, podClusterCritical)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not evict static pods", func() {
			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			ExpectApplied(ctx, env.Client, node, podEvict)

			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
					}},
				},
			})
			ExpectApplied(ctx, env.Client, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect mirror pod to not be queued for eviction
			ExpectNotEnqueuedForEviction(evictionQueue, podNoEvict)

			// Expect podEvict to be enqueued for eviction then be successful
			ExpectEvicted(env.Client, podEvict)

			// Expect node to exist and be draining
			ExpectNodeDraining(env.Client, node.Name)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)

		})
		It("should not delete nodes until all pods are deleted", func() {
			pods := []*v1.Pod{
				test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}}),
				test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}}),
			}
			ExpectApplied(ctx, env.Client, node, pods[0], pods[1])

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))

			// Expect the pods to be evicted
			ExpectEvicted(env.Client, pods[0], pods[1])

			// Expect node to exist and be draining, but not deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNodeDraining(env.Client, node.Name)

			ExpectDeleted(ctx, env.Client, pods[1])

			// Expect node to exist and be draining, but not deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNodeDraining(env.Client, node.Name)

			ExpectDeleted(ctx, env.Client, pods[0])

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should wait for pods to terminate", func() {
			pod := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			fakeClock.SetTime(time.Now()) // make our fake clock match the pod creation time
			ExpectApplied(ctx, env.Client, node, pod)

			// Before grace period, node should not delete
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectEvicted(env.Client, pod)

			// After grace period, node should delete. The deletion timestamps are from etcd which we can't control, so
			// to eliminate test-flakiness we reset the time to current time + 90 seconds instead of just advancing
			// the clock by 90 seconds.
			fakeClock.SetTime(time.Now().Add(90 * time.Second))
			ExpectReconcileSucceeded(ctx, controller, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
	})
})

func ExpectNotEnqueuedForEviction(e *termination.EvictionQueue, pods ...*v1.Pod) {
	for _, pod := range pods {
		ExpectWithOffset(1, e.Contains(client.ObjectKeyFromObject(pod))).To(BeFalse())
	}
}

func ExpectEvicted(c client.Client, pods ...*v1.Pod) {
	for _, pod := range pods {
		EventuallyWithOffset(1, func() bool {
			return ExpectPodExists(ctx, c, pod.Name, pod.Namespace).GetDeletionTimestamp().IsZero()
		}, ReconcilerPropagationTime, RequestInterval).Should(BeFalse(), func() string {
			return fmt.Sprintf("expected %s/%s to be evicting, but it isn't", pod.Namespace, pod.Name)
		})
	}
}

func ExpectNodeDraining(c client.Client, nodeName string) *v1.Node {
	node := ExpectNodeExistsWithOffset(1, ctx, c, nodeName)
	ExpectWithOffset(1, node.Spec.Unschedulable).To(BeTrue())
	ExpectWithOffset(1, lo.Contains(node.Finalizers, v1alpha5.TerminationFinalizer)).To(BeTrue())
	ExpectWithOffset(1, node.DeletionTimestamp.IsZero()).To(BeFalse())
	return node
}
