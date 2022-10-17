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

package common

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,stylecheck
	. "github.com/onsi/gomega"    //nolint:revive,stylecheck
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1beta1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/utils/functional"
	nodeutils "github.com/aws/karpenter/pkg/utils/node"
	"github.com/aws/karpenter/pkg/utils/pod"
)

var (
	//nolint:govet
	CleanableObjects = []functional.Pair[client.Object, client.ObjectList]{
		{&v1.Pod{}, &v1.PodList{}},
		{&appsv1.Deployment{}, &appsv1.DeploymentList{}},
		{&appsv1.DaemonSet{}, &appsv1.DaemonSetList{}},
		{&policyv1.PodDisruptionBudget{}, &policyv1.PodDisruptionBudgetList{}},
		{&v1.PersistentVolumeClaim{}, &v1.PersistentVolumeClaimList{}},
		{&v1.PersistentVolume{}, &v1.PersistentVolumeList{}},
		{&storagev1.StorageClass{}, &storagev1.StorageClassList{}},
		{&v1alpha5.Provisioner{}, &v1alpha5.ProvisionerList{}},
	}
)

const (
	NoWatch  = "NoWatch"
	NoEvents = "NoEvents"
)

var testStartTime time.Time
var stop chan struct{}

// nolint:gocyclo
func (env *Environment) BeforeEach(opts ...Option) {
	options := ResolveOptions(opts)
	if options.EnableDebug {
		fmt.Println("------- START BEFORE -------")
		defer fmt.Println("------- END BEFORE -------")
	}

	stop = make(chan struct{})
	testStartTime = time.Now()

	var nodes v1.NodeList
	Expect(env.Client.List(env.Context, &nodes)).To(Succeed())
	if options.EnableDebug {
		for i := range nodes.Items {
			fmt.Println(env.getNodeInformation(&nodes.Items[i]))
		}
	}
	for _, node := range nodes.Items {
		if len(node.Spec.Taints) == 0 && !node.Spec.Unschedulable {
			Fail(fmt.Sprintf("expected system pool node %s to be tainted", node.Name))
		}
	}

	var pods v1.PodList
	Expect(env.Client.List(env.Context, &pods)).To(Succeed())
	if options.EnableDebug {
		for i := range pods.Items {
			fmt.Println(getPodInformation(&pods.Items[i]))
		}
	}
	for i := range pods.Items {
		Expect(pod.IsProvisionable(&pods.Items[i])).To(BeFalse(),
			fmt.Sprintf("expected to have no provisionable pods, found %s/%s", pods.Items[i].Namespace, pods.Items[i].Name))
		Expect(pods.Items[i].Namespace).ToNot(Equal("default"),
			fmt.Sprintf("expected no pods in the `default` namespace, found %s/%s", pods.Items[i].Namespace, pods.Items[i].Name))
	}
	// If the test is labeled as NoWatch, then the node/pod monitor will just list at the beginning
	// of the test rather than perform a watch during it
	if options.EnableDebug && !lo.Contains(CurrentSpecReport().Labels(), NoWatch) {
		env.startNodeMonitor(stop)
		env.startPodMonitor(stop)
	}
	var provisioners v1alpha5.ProvisionerList
	Expect(env.Client.List(env.Context, &provisioners)).To(Succeed())
	Expect(provisioners.Items).To(HaveLen(0), "expected no provisioners to exist")
	env.Monitor.Reset()
	env.StartingNodeCount = env.Monitor.NodeCountAtReset()
}

func (env *Environment) getNodeInformation(n *v1.Node) string {
	pods, _ := nodeutils.GetNodePods(env, env.Client, n)
	return fmt.Sprintf("node %s ready=%s schedulable=%t initialized=%s pods=%d taints=%v", n.Name, nodeutils.GetCondition(n, v1.NodeReady).Status, !n.Spec.Unschedulable, n.Labels[v1alpha5.LabelNodeInitialized], len(pods), n.Spec.Taints)
}

func getPodInformation(p *v1.Pod) string {
	var containerInfo strings.Builder
	for _, c := range p.Status.ContainerStatuses {
		if containerInfo.Len() > 0 {
			fmt.Fprintf(&containerInfo, ", ")
		}
		fmt.Fprintf(&containerInfo, "%s restarts=%d", c.Name, c.RestartCount)
	}
	return fmt.Sprintf("pods %s/%s provisionable=%v phase=%s nodename=%s [%s]", p.Namespace, p.Name,
		pod.IsProvisionable(p), p.Status.Phase, p.Spec.NodeName, containerInfo.String())
}

// Partially copied from
// https://github.com/kubernetes/kubernetes/blob/04ee339c7a4d36b4037ce3635993e2a9e395ebf3/staging/src/k8s.io/kubectl/pkg/describe/describe.go#L4232
func getEventInformation(kind string, k types.NamespacedName, el *v1.EventList) string {
	sb := strings.Builder{}
	sb.WriteString(fmt.Sprintf("------- %s/%s%s EVENTS -------\n",
		kind, lo.Ternary(k.Namespace != "", k.Namespace+"/", ""), k.Name))
	if len(el.Items) == 0 {
		return sb.String()
	}
	for _, e := range el.Items {
		source := e.Source.Component
		if source == "" {
			source = e.ReportingController
		}
		eventTime := e.EventTime
		if eventTime.IsZero() {
			eventTime = metav1.NewMicroTime(e.FirstTimestamp.Time)
		}
		sb.WriteString(fmt.Sprintf("time=%s type=%s reason=%s from=%s message=%s\n",
			eventTime.Format(time.RFC3339),
			e.Type,
			e.Reason,
			source,
			strings.TrimSpace(e.Message)),
		)
	}
	return sb.String()
}

// startPodMonitor monitors all pods that are provisioned in a namespace outside kube-system
// and karpenter namespaces during a test
func (env *Environment) startPodMonitor(stop <-chan struct{}) {
	factory := informers.NewSharedInformerFactoryWithOptions(env.KubeClient, time.Second*30,
		informers.WithTweakListOptions(func(l *metav1.ListOptions) {
			l.FieldSelector = "metadata.namespace!=kube-system,metadata.namespace!=karpenter"
		}))
	podInformer := factory.Core().V1().Pods().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fmt.Printf("[CREATED %s] %s\n", time.Now().Format(time.RFC3339), getPodInformation(obj.(*v1.Pod)))
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			if getPodInformation(oldObj.(*v1.Pod)) != getPodInformation(newObj.(*v1.Pod)) {
				fmt.Printf("[UPDATED %s] %s\n", time.Now().Format(time.RFC3339), getPodInformation(newObj.(*v1.Pod)))
			}
		},
		DeleteFunc: func(obj interface{}) {
			fmt.Printf("[DELETED %s] %s\n", time.Now().Format(time.RFC3339), getPodInformation(obj.(*v1.Pod)))
		},
	})
	factory.Start(stop)
}

// startNodeMonitor monitors all nodes that are provisioned by any provisioners during a test
func (env *Environment) startNodeMonitor(stop <-chan struct{}) {
	factory := informers.NewSharedInformerFactoryWithOptions(env.KubeClient, time.Second*30,
		informers.WithTweakListOptions(func(l *metav1.ListOptions) { l.LabelSelector = v1alpha5.ProvisionerNameLabelKey }))
	podInformer := factory.Core().V1().Nodes().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*v1.Node)
			if _, ok := node.Labels[TestLabelName]; ok {
				fmt.Printf("[CREATED %s] %s\n", time.Now().Format(time.RFC3339), env.getNodeInformation(obj.(*v1.Node)))
			}
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			if env.getNodeInformation(oldObj.(*v1.Node)) != env.getNodeInformation(newObj.(*v1.Node)) {
				fmt.Printf("[UPDATED %s] %s\n", time.Now().Format(time.RFC3339), env.getNodeInformation(newObj.(*v1.Node)))
			}
		},
		DeleteFunc: func(obj interface{}) {
			fmt.Printf("[DELETED %s] %s\n", time.Now().Format(time.RFC3339), env.getNodeInformation(obj.(*v1.Node)))
		},
	})
	factory.Start(stop)
}

func (env *Environment) AfterEach(opts ...Option) {
	options := ResolveOptions(opts)
	if options.EnableDebug {
		fmt.Println("------- START AFTER -------")
		defer fmt.Println("------- END AFTER -------")
	}

	env.CleanupObjects(CleanableObjects, options)
	env.eventuallyExpectScaleDown()
	env.expectNoCrashes()
}

func (env *Environment) DumpLogs(opts ...Option) {
	options := ResolveOptions(opts)
	close(stop) // close the pod/node monitor watch channel

	if options.EnableDebug && !lo.Contains(CurrentSpecReport().Labels(), NoEvents) {
		env.dumpPodEvents(testStartTime)
		env.dumpNodeEvents(testStartTime)
	}
	env.printControllerLogs(&v1.PodLogOptions{Container: "controller"})
}

func (env *Environment) CleanupObjects(cleanableObjects []functional.Pair[client.Object, client.ObjectList], options Options) {
	namespaces := &v1.NamespaceList{}
	Expect(env.Client.List(env, namespaces)).To(Succeed())
	wg := sync.WaitGroup{}
	for _, p := range cleanableObjects {
		if !lo.ContainsBy(options.IgnoreCleanupFor, func(o client.Object) bool { return reflect.TypeOf(o) == reflect.TypeOf(p.First) }) {
			for _, namespace := range namespaces.Items {
				wg.Add(1)
				go func(obj client.Object, objList client.ObjectList, namespace string) {
					defer GinkgoRecover()
					defer wg.Done()
					Expect(env.Client.DeleteAllOf(env, obj,
						client.InNamespace(namespace),
						client.HasLabels([]string{TestLabelName}),
						client.PropagationPolicy(metav1.DeletePropagationForeground),
					)).To(Succeed())
					Eventually(func(g Gomega) {
						stored := objList.DeepCopyObject().(client.ObjectList)
						g.Expect(env.Client.List(env, stored,
							client.InNamespace(namespace),
							client.HasLabels([]string{TestLabelName}))).To(Succeed())
						items, err := meta.ExtractList(objList)
						g.Expect(err).To(Succeed())
						g.Expect(len(items)).To(BeZero())
					}).Should(Succeed())
				}(p.First, p.Second, namespace.Name)
			}
		}
	}
	wg.Wait()
}

func (env *Environment) dumpPodEvents(testStartTime time.Time) {
	el := &v1.EventList{}
	ExpectWithOffset(1, env.Client.List(env, el, &client.ListOptions{
		FieldSelector: fields.SelectorFromSet(map[string]string{"involvedObject.kind": "Pod"}),
	})).To(Succeed())

	eventMap := map[types.NamespacedName]*v1.EventList{}

	filteredEvents := lo.Filter(el.Items, func(e v1.Event, _ int) bool {
		if !e.EventTime.IsZero() {
			if e.EventTime.BeforeTime(&metav1.Time{Time: testStartTime}) {
				return false
			}
		} else if e.FirstTimestamp.Before(&metav1.Time{Time: testStartTime}) {
			return false
		}
		if e.InvolvedObject.Namespace == "kube-system" || e.InvolvedObject.Namespace == "karpenter" {
			return false
		}
		return true
	})
	for i := range filteredEvents {
		elem := filteredEvents[i]
		objectKey := types.NamespacedName{Namespace: elem.InvolvedObject.Namespace, Name: elem.InvolvedObject.Name}
		if _, ok := eventMap[objectKey]; !ok {
			eventMap[objectKey] = &v1.EventList{}
		}
		eventMap[objectKey].Items = append(eventMap[objectKey].Items, elem)
	}
	for k, v := range eventMap {
		fmt.Print(getEventInformation("pod", k, v))
	}
}

func (env *Environment) dumpNodeEvents(testStartTime time.Time) {
	nodeNames := sets.NewString(lo.Map(env.Monitor.CreatedNodes(), func(n *v1.Node, _ int) string { return n.Name })...)

	el := &v1.EventList{}
	ExpectWithOffset(1, env.Client.List(env, el, &client.ListOptions{
		FieldSelector: fields.SelectorFromSet(map[string]string{"involvedObject.kind": "Node"}),
	})).To(Succeed())

	eventMap := map[types.NamespacedName]*v1.EventList{}

	filteredEvents := lo.Filter(el.Items, func(e v1.Event, _ int) bool {
		if !e.EventTime.IsZero() {
			if e.EventTime.BeforeTime(&metav1.Time{Time: testStartTime}) {
				return false
			}
		} else if e.FirstTimestamp.Before(&metav1.Time{Time: testStartTime}) {
			return false
		}
		if !nodeNames.Has(e.InvolvedObject.Name) {
			return false
		}
		return true
	})
	for i := range filteredEvents {
		elem := filteredEvents[i]
		objectKey := types.NamespacedName{Namespace: elem.InvolvedObject.Namespace, Name: elem.InvolvedObject.Name}
		if _, ok := eventMap[objectKey]; !ok {
			eventMap[objectKey] = &v1.EventList{}
		}
		eventMap[objectKey].Items = append(eventMap[objectKey].Items, elem)
	}
	for k, v := range eventMap {
		fmt.Print(getEventInformation("node", k, v))
	}
}
