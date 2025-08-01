---
title: "Disruption"
linkTitle: "Disruption"
weight: 50
description: >
  Understand different ways Karpenter disrupts nodes
---

## Control Flow

Karpenter sets a Kubernetes [finalizer](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/) on each node and node claim it provisions.
The finalizer blocks deletion of the node object while the Termination Controller taints and drains the node, before removing the underlying NodeClaim. Disruption is triggered by the Disruption Controller, by the user through manual disruption, or through an external system that sends a delete request to the node object.

### Disruption Controller

Karpenter automatically discovers disruptable nodes and spins up replacements when needed. Karpenter disrupts nodes by executing one [automated method](#automated-graceful-methods) at a time, first doing Drift then Consolidation. Each method varies slightly, but they all follow the standard disruption process. Karpenter uses [disruption budgets]({{<ref "#nodepool-disruption-budgets" >}}) to control the speed at which these disruptions begin.
1. Identify a list of prioritized candidates for the disruption method.
   * If there are [pods that cannot be evicted](#pod-level-controls) on the node, Karpenter will ignore the node and try disrupting it later.
   * If there are no disruptable nodes, continue to the next disruption method.
2. For each disruptable node:
   1. Check if disrupting it would violate its NodePool's disruption budget.
   2. Execute a scheduling simulation with the pods on the node to find if any replacement nodes are needed.
3. Add the `karpenter.sh/disrupted:NoSchedule` taint to the node(s) to prevent pods from scheduling to it.
4. Pre-spin any replacement nodes needed as calculated in Step (2), and wait for them to become ready.
   * If a replacement node fails to initialize, un-taint the node(s), and restart from Step (1), starting at the first disruption method again.
5. Delete the node(s) and wait for the Termination Controller to gracefully shutdown the node(s).
6. Once the Termination Controller terminates the node, go back to Step (1), starting at the first disruption method again.

### Termination Controller

When a Karpenter node is deleted, the Karpenter finalizer will block deletion and the APIServer will set the `DeletionTimestamp` on the node, allowing Karpenter to gracefully shutdown the node, modeled after [Kubernetes Graceful Node Shutdown](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/#graceful-node-shutdown). Karpenter's graceful shutdown process will:
1. Add the `karpenter.sh/disrupted:NoSchedule` taint to the node to prevent pods from scheduling to it.
2. Begin evicting the pods on the node with the [Kubernetes Eviction API](https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/) to respect PDBs, while ignoring all [static pods](https://kubernetes.io/docs/tasks/configure-pod-container/static-pod/), pods tolerating the `karpenter.sh/disrupted:NoSchedule` taint, and succeeded/failed pods. Wait for the node to be fully drained before proceeding to Step (3).
   * While waiting, if the underlying NodeClaim for the node no longer exists, remove the finalizer to allow the APIServer to delete the node, completing termination.
3. Terminate the NodeClaim in the Cloud Provider.
4. Remove the finalizer from the node to allow the APIServer to delete the node, completing termination.

## Manual Methods
* **Node Deletion**: You can use `kubectl` to manually remove a single Karpenter node or nodeclaim. Since each Karpenter node is owned by a NodeClaim, deleting either the node or the nodeclaim will cause cascade deletion of the other:

    ```bash
    # Delete a specific nodeclaim
    kubectl delete nodeclaim $NODECLAIM_NAME

    # Delete a specific node
    kubectl delete node $NODE_NAME

    # Delete all nodeclaims
    kubectl delete nodeclaims --all

    # Delete all nodes owned by any nodepool
    kubectl delete nodes -l karpenter.sh/nodepool

    # Delete all nodeclaims owned by a specific nodepoolXS
    kubectl delete nodeclaims -l karpenter.sh/nodepool=$NODEPOOL_NAME
    ```
* **NodePool Deletion**: NodeClaims are owned by the NodePool through an [owner reference](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/#owner-references-in-object-specifications) that launched them. Karpenter will gracefully terminate nodes through cascading deletion when the owning NodePool is deleted.

{{% alert title="Note" color="primary" %}}
By adding the finalizer, Karpenter improves the default Kubernetes process of node deletion.
When you run `kubectl delete node` on a node without a finalizer, the node is deleted without triggering the finalization logic. The instance will continue running in EC2, even though there is no longer a node object for it. The kubelet isn’t watching for its own existence, so if a node is deleted, the kubelet doesn’t terminate itself. All the pod objects get deleted by a garbage collection process later, because the pods’ node is gone.
{{% /alert %}}

## Automated Graceful Methods

Automated graceful methods, can be rate limited through [NodePool Disruption Budgets]({{<ref "#nodepool-disruption-budgets" >}})

* [**Consolidation**]({{<ref "#consolidation" >}}): Karpenter works to actively reduce cluster cost by identifying when:
  * Nodes can be removed because the node is empty
  * Nodes can be removed as their workloads will run on other nodes in the cluster.
  * Nodes can be replaced with lower priced variants due to a change in the workloads.
* [**Drift**]({{<ref "#drift" >}}): Karpenter will mark nodes as drifted and disrupt nodes that have drifted from their desired specification. See [Drift]({{<ref "#drift" >}}) to see which fields are considered.

{{% alert title="Defaults" color="secondary" %}}
Disruption is configured through the NodePool's disruption block by the `consolidationPolicy`, and `consolidateAfter` fields. Karpenter will configure these fields with the following values by default if they are not set:

```yaml
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
```
{{% /alert %}}

### Consolidation

Consolidation is configured by `consolidationPolicy` and `consolidateAfter`. `consolidationPolicy` determines the pre-conditions for nodes to be considered consolidatable, and are `WhenEmpty` or `WhenEmptyOrUnderutilized`. If a node has no running non-daemon pods, it is considered empty.  `consolidateAfter` can be set to indicate how long Karpenter should wait after a pod schedules or is removed from the node before considering the node consolidatable. With `WhenEmptyOrUnderutilized`, Karpenter will consider a node consolidatable when its `consolidateAfter` has been reached, empty or not.

Karpenter has two mechanisms for cluster consolidation:
1. **Deletion** - A node is eligible for deletion if all of its pods can run on free capacity of other nodes in the cluster.
2. **Replace** - A node can be replaced if all of its pods can run on a combination of free capacity of other nodes in the cluster and a single lower price replacement node.

Consolidation has three mechanisms that are performed in order to attempt to identify a consolidation action:
1. **Empty Node Consolidation** - Delete any entirely empty nodes in parallel
2. **Multi Node Consolidation** - Try to delete two or more nodes in parallel, possibly launching a single replacement whose price is lower than that of all nodes being removed
3. **Single Node Consolidation** - Try to delete any single node, possibly launching a single replacement whose price is lower than that of the node being removed

It's impractical to examine all possible consolidation options for multi-node consolidation, so Karpenter uses a heuristic to identify a likely set of nodes that can be consolidated.  For single-node consolidation we consider each node in the cluster individually.

When there are multiple nodes that could be potentially deleted or replaced, Karpenter chooses to consolidate the node that overall disrupts your workloads the least by preferring to terminate:

* Nodes running fewer pods
* Nodes that will expire soon
* Nodes with lower priority pods

If consolidation is enabled, Karpenter periodically reports events against nodes that indicate why the node can't be consolidated.  These events can be used to investigate nodes that you expect to have been consolidated, but still remain in your cluster.

```bash
Events:
  Type     Reason                   Age                From             Message
  ----     ------                   ----               ----             -------
  Normal   Unconsolidatable         66s                karpenter        pdb default/inflate-pdb prevents pod evictions
  Normal   Unconsolidatable         33s (x3 over 30m)  karpenter        can't replace with a lower-priced node
```

{{% alert title="Warning" color="warning" %}}
Using preferred anti-affinity and topology spreads can reduce the effectiveness of consolidation. At node launch, Karpenter attempts to satisfy affinity and topology spread preferences. In order to reduce node churn, consolidation must also attempt to satisfy these constraints to avoid immediately consolidating nodes after they launch. This means that consolidation may not disrupt nodes in order to avoid violating preferences, even if kube-scheduler can fit the host pods elsewhere.  Karpenter reports these pods via logging to bring awareness to the possible issues they can cause (e.g. `pod default/inflate-anti-self-55894c5d8b-522jd has a preferred Anti-Affinity which can prevent consolidation`).
{{% /alert %}}

#### Spot consolidation
For spot nodes, Karpenter has deletion consolidation enabled by default. If you would like to enable replacement with spot consolidation, you need to enable the feature through the [`SpotToSpotConsolidation` feature flag]({{<ref "../reference/settings#features-gates" >}}).

Lower priced spot instance types are selected with the [`price-capacity-optimized` strategy](https://aws.amazon.com/blogs/compute/introducing-price-capacity-optimized-allocation-strategy-for-ec2-spot-instances/). Sometimes, the lowest priced spot instance type is not launched due to the likelihood of interruption. As a result, Karpenter uses the number of available instance type options with a price lower than the currently launched spot instance as a heuristic for evaluating whether it should launch a replacement for the current spot node.

We refer to the number of instances that Karpenter has within its launch decision as a launch's "instance type flexibility." When Karpenter is considering performing a spot-to-spot consolidation replacement, it will check whether replacing the instance type will lead to enough instance type flexibility in the subsequent launch request. As a result, we get the following properties when evaluating for consolidation:
1) We shouldn't continually consolidate down to the lowest priced spot instance which might have very high rates of interruption.
2) We launch with enough instance types that there’s high likelihood that our replacement instance has comparable availability to our current one.

Karpenter requires a minimum instance type flexibility of 15 instance types when performing single node spot-to-spot consolidations (1 node to 1 node). It does not have the same instance type flexibility requirement for multi-node spot-to-spot consolidations (many nodes to 1 node) since doing so without requiring flexibility won't lead to "race to the bottom" scenarios.


### Drift
Drift handles changes to the NodePool/EC2NodeClass. For Drift, values in the NodePool/EC2NodeClass are reflected in the NodeClaimTemplateSpec/EC2NodeClassSpec in the same way that they’re set. A NodeClaim will be detected as drifted if the values in its owning NodePool/EC2NodeClass do not match the values in the NodeClaim. Similar to the upstream `deployment.spec.template` relationship to pods, Karpenter will annotate the owning NodePool and EC2NodeClass with a hash of the NodeClaimTemplateSpec to check for drift. Some special cases will be discovered either from Karpenter or through the CloudProvider interface, triggered by NodeClaim/Instance/NodePool/EC2NodeClass changes.

#### Special Cases on Drift
In special cases, drift can correspond to multiple values and must be handled differently. Drift on resolved fields can create cases where drift occurs without changes to CRDs, or where CRD changes do not result in drift. For example, if a NodeClaim has `node.kubernetes.io/instance-type: m5.large`, and requirements change from `node.kubernetes.io/instance-type In [m5.large]` to `node.kubernetes.io/instance-type In [m5.large, m5.2xlarge]`, the NodeClaim will not be drifted because its value is still compatible with the new requirements. Conversely, if a NodeClaim is using a NodeClaim image `ami: ami-abc`, but a new image is published, Karpenter's `EC2NodeClass.spec.amiSelectorTerms` will discover that the new correct value is `ami: ami-xyz`, and detect the NodeClaim as drifted.

##### NodePool
| Fields         |
|----------------|
| spec.template.spec.requirements   |

##### EC2NodeClass
| Fields                        |
|-------------------------------|
| spec.subnetSelectorTerms      |
| spec.securityGroupSelectorTerms  |
| spec.amiSelectorTerms  |

#### Behavioral Fields
Behavioral Fields are treated as over-arching settings on the NodePool to dictate how Karpenter behaves. These fields don’t correspond to settings on the NodeClaim or instance. They’re set by the user to control Karpenter’s Provisioning and disruption logic. Since these don’t map to a desired state of NodeClaims, __behavioral fields are not considered for Drift__.

##### NodePool
| Fields              |
|---------------------|
| spec.weight         |
| spec.limits         |
| spec.disruption.*   |

Read the [Drift Design](https://github.com/aws/karpenter-core/blob/main/designs/drift.md) for more.


Karpenter will add the `Drifted` status condition on NodeClaims if the NodeClaim is drifted from its owning NodePool. Karpenter will also remove the `Drifted` status condition if either:
1. The `Drift` feature gate is not enabled but the NodeClaim is drifted, Karpenter will remove the status condition.
2. The NodeClaim isn't drifted, but has the status condition, Karpenter will remove it.

## Automated Forceful Methods

Automated forceful methods will begin draining nodes as soon as the condition is met.
Unlike the graceful methods mentioned above, these methods can not be rate-limited using [NodePool Disruption Budgets](#nodepool-disruption-budgets), and do not wait for a pre-spin replacement node to be healthy for the pods to reschedule.
Pod disruption budgets may be used to rate-limit application disruption.

### Expiration

A node is expired once it's lifetime exceeds the duration set on the owning NodeClaim's `spec.expireAfter` field.
Changes to `spec.template.spec.expireAfter` on the owning NodePool will not update the field for existing NodeClaims - it will induce NodeClaim drift and the replacements will have the updated value.
Expiration can be used, in conjunction with [`terminationGracePeriod`](#termination-grace-period), to enforce a maximum Node lifetime.
By default, `expireAfter` is set to `720h` (30 days).

{{% alert title="Warning" color="warning" %}}
Misconfigured PDBs and pods with the `karpenter.sh/do-not-disrupt` annotation may block draining indefinitely.
For this reason, it is not recommended to set `expireAfter` without also setting `terminationGracePeriod` **if** your cluster has pods with the `karpenter.sh/do-not-disrupt` annotation.
Doing so can result in partially drained nodes stuck in the cluster, driving up cluster cost and potentially requiring manual intervention to resolve.
{{% /alert %}}

### Interruption

If interruption-handling is enabled, Karpenter will watch for upcoming involuntary interruption events that would cause disruption to your workloads. These interruption events include:

* Spot Interruption Warnings
* Scheduled Change Health Events (Maintenance Events)
* Instance Terminating Events
* Instance Stopping Events

When Karpenter detects one of these events will occur to your nodes, it automatically taints, drains, and terminates the node(s) ahead of the interruption event to give the maximum amount of time for workload cleanup prior to compute disruption. This enables scenarios where the `terminationGracePeriod` for your workloads may be long or cleanup for your workloads is critical, and you want enough time to be able to gracefully clean-up your pods.

For Spot interruptions, the NodePool will start a new node as soon as it sees the Spot interruption warning. Spot interruptions have a __2 minute notice__ before Amazon EC2 reclaims the instance. Once Karpenter has received this warning it will begin draining the node while in parallel provisioning a new node. Karpenter's average node startup time means that, generally, there is sufficient time for the new node to become ready before EC2 initiates termination for the spot instance.

{{% alert title="Note" color="primary" %}}
Karpenter publishes Kubernetes events to the node for all events listed above in addition to [__Spot Rebalance Recommendations__](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/rebalance-recommendations.html). Karpenter does not currently support taint, drain, and terminate logic for Spot Rebalance Recommendations.

If you require handling for Spot Rebalance Recommendations, you can use the [AWS Node Termination Handler (NTH)](https://github.com/aws/aws-node-termination-handler) alongside Karpenter; however, note that the AWS Node Termination Handler cordons and drains nodes on rebalance recommendations, potentially causing more node churn in the cluster than with interruptions alone. Further information can be found in the [Troubleshooting Guide]({{< ref "../troubleshooting#aws-node-termination-handler-nth-interactions" >}}).
{{% /alert %}}

Karpenter enables this feature by watching an SQS queue which receives critical events from AWS services which may affect your nodes. Karpenter requires that an SQS queue be provisioned and EventBridge rules and targets be added that forward interruption events from AWS services to the SQS queue. Karpenter provides details for provisioning this infrastructure in the [CloudFormation template in the Getting Started Guide](../../getting-started/getting-started-with-karpenter/#create-the-karpenter-infrastructure-and-iam-roles).

To enable interruption handling, configure the `--interruption-queue` CLI argument with the name of the interruption queue provisioned to handle interruption events.

### Node Auto Repair

<i class="fa-solid fa-circle-info"></i> <b>Feature State: </b> Karpenter v1.1.0 [alpha]({{<ref "../reference/settings#feature-gates" >}})

Node Auto Repair is a feature that automatically identifies and replaces unhealthy nodes in your cluster, helping to maintain overall cluster health. Nodes can experience various types of failures affecting their hardware, file systems, or container environments. These failures may be surfaced through node conditions such as network unavailability, disk pressure, memory pressure, or other conditions reported by node diagnostic agents. When Karpenter detects these unhealthy conditions, it automatically replaces the affected nodes based on cloud provider-defined repair policies. Once a node has been in an unhealthy state beyond its configured toleration duration, Karpenter will forcefully terminate the node and its corresponding NodeClaim, bypassing the standard drain and grace period procedures to ensure swift replacement of problematic nodes. To prevent cascading failures, Karpenter includes safety mechanisms: it will not perform repairs if more than 20% of nodes in a NodePool are unhealthy, and for standalone NodeClaims, it evaluates this threshold against all nodes in the cluster. This ensures your cluster remains in a healthy state with minimal manual intervention, even in scenarios where normal node termination procedures might be impacted by the node's unhealthy state.

To enable Node Auto Repair:
  1.  Ensure you have a [Node Monitoring Agent](https://docs.aws.amazon.com/en_us/eks/latest/userguide/node-health.html) deployed or any agent that will add status conditions to nodes that are supported (e.g., Node Problem Detector)
  2.  Enable the feature flag: `NodeRepair=true`
  3. Node AutoRepair will automatically terminate nodes when they have unhealthy status conditions based on your cloud provider's repair policies


Karpenter monitors nodes for the following node status conditions when initiating repair actions:


#### Kubelet Node Conditions

|   Type  |    Status     | Toleration Duration |
| ------  | ------------- | ------------------- |
|  Ready  |     False     |     30 minutes      |
|  Ready  |     Unknown   |     30 minutes      |

#### Node Monitoring Agent Conditions

|            Type            |    Status     | Toleration Duration |
| ------------------------   | ------------| --------------------- |
|  AcceleratedHardwareReady  |     False   |     10 minutes        |
|  StorageReady              |     False   |     30 minutes        |
|  NetworkingReady           |     False   |     30 minutes        |
|  KernelReady               |     False   |     30 minutes        |
|  ContainerRuntimeReady     |     False   |     30 minutes        |

To enable the NodeRepair feature flag, refer to the [Feature Gates]({{<ref "../reference/settings#feature-gates" >}}).

## Controls

### TerminationGracePeriod

To configure a maximum termination duration, `terminationGracePeriod` should be used.
It is configured through a NodePool's [`spec.template.spec.terminationGracePeriod`]({{<ref "../concepts/nodepools/#spectemplatespecterminationgraceperiod" >}}) field, and is persisted to created NodeClaims (`spec.terminationGracePeriod`).
Changes to the [`spec.template.spec.terminationGracePeriod`]({{<ref "../concepts/nodepools/#spectemplatespecterminationgraceperiod" >}}) field on the NodePool will not result in a change for existing NodeClaims - it will induce NodeClaim drift and the replacements will have the updated `terminationGracePeriod`.

Once a node is disrupted, via either a [graceful](#automated-graceful-methods) or [forceful](#automated-forceful-methods) disruption method, Karpenter will begin draining the node.
At this point, the countdown for `terminationGracePeriod` begins.
Once the `terminationGracePeriod` elapses, remaining pods will be forcibly deleted and the underlying instance will be terminated.
A node may be terminated before the `terminationGracePeriod` has elapsed if all disruptable pods have been drained.

In conjunction with `expireAfter`, `terminationGracePeriod` can be used to enforce an absolute maximum node lifetime.
The node will begin to drain once its `expireAfter` has elapsed, and it will be forcibly terminated once its `terminationGracePeriod` has elapsed, making the maximum node lifetime the sum of the two fields.

Additionally, configuring `terminationGracePeriod` changes the eligibility criteria for disruption via `Drift`.
When configured, a node may be disrupted via drift even if there are pods with blocking PDBs or the `karpenter.sh/do-not-disrupt` annotation scheduled to it.
This enables cluster administrators to ensure crucial updates (e.g. AMI updates addressing CVEs) can't be blocked by misconfigured applications.

{{% alert title="Warning" color="warning" %}}
To ensure that the `terminationGracePeriodSeconds` value for draining pods is respected, pods will be preemptively deleted before the Node's `terminationGracePeriod` has elapsed.
This includes pods with blocking [pod disruption budgets](https://kubernetes.io/docs/tasks/run-application/configure-pdb/) or the [`karpenter.sh/do-not-disrupt` annotation]({{<ref "#pod-level-controls" >}}).

Consider the following example: a Node with a 1 hour `terminationGracePeriod` has been disrupted and begins to drain.
A pod with the `karpenter.sh/do-not-disrupt` annotation and a 300 second (5 minute) `terminationGracePeriodsSeconds` is scheduled to it.
If the pod is still running 55 minutes after the Node begins to drain, the pod will be deleted to ensure its `terminationGracePeriodSeconds` value is respected.

If a pod's `terminationGracePeriodSeconds` value exceeds that of the Node it is scheduled to, Karpenter will prioritize the Node's `terminationGracePeriod`.
The pod will be deleted as soon as the Node begins to drain, and it will not receive it's full `terminationGracePeriodSeconds`.
{{% /alert %}}

### NodePool Disruption Budgets

You can rate limit Karpenter's disruption through the NodePool's `spec.disruption.budgets`. If undefined, Karpenter will default to one budget with `nodes: 10%`. Budgets will consider nodes that are actively being deleted for any reason, and will only block Karpenter from disrupting nodes voluntarily through drift, emptiness, and consolidation. Note that NodePool Disruption Budgets do not prevent Karpenter from terminating expired nodes.

#### Reasons
Karpenter allows specifying if a budget applies to any of `Drifted`, `Underutilized`, or `Empty`. When a budget has no reasons, it's assumed that it applies to all reasons. When calculating allowed disruptions for a given reason, Karpenter will take the minimum of the budgets that have listed the reason or have left reasons undefined.

#### Nodes
When calculating if a budget will block nodes from disruption, Karpenter lists the total number of nodes owned by a NodePool, subtracting out the nodes owned by that NodePool that are currently being deleted and nodes that are NotReady. If the number of nodes being deleted by Karpenter or any other processes is greater than the number of allowed disruptions, disruption for this node will not proceed.

If the budget is configured with a percentage value, such as `20%`, Karpenter will calculate the number of allowed disruptions as `allowed_disruptions = roundup(total * percentage) - total_deleting - total_notready`. If otherwise defined as a non-percentage value, Karpenter will simply use that number as a static ceiling `non_percentage_value - total_deleting - total_notready`. For multiple budgets in a NodePool, Karpenter will take the minimum value (most restrictive) of each of the budgets.

For example, the following NodePool with three budgets defines the following requirements:
- The first budget will only allow 20% of nodes owned by that NodePool to be disrupted if it's empty or drifted. For instance, if there were 19 nodes owned by the NodePool, 4 empty or drifted nodes could be disrupted, rounding up from `19 * .2 = 3.8`.
- The second budget acts as a ceiling to the previous budget, only allowing 5 disruptions when there are more than 25 nodes.
- The last budget only blocks disruptions during the first 10 minutes of the day, where 0 disruptions are allowed, only applying to underutilized nodes.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    budgets:
    - nodes: "20%"
      reasons:
      - "Empty"
      - "Drifted"
    - nodes: "5"
    - nodes: "0"
      schedule: "@daily"
      duration: 10m
      reasons:
      - "Underutilized"
```

#### Schedule
Schedule is a cronjob schedule. Generally, the cron syntax is five space-delimited values with options below, with additional special macros like `@yearly`, `@monthly`, `@weekly`, `@daily`, `@hourly`.
Follow the [Kubernetes documentation](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/#writing-a-cronjob-spec) for more information on how to follow the cron syntax. Timezones are not currently supported. Schedules are always in UTC.

```bash
# ┌───────────── minute (0 - 59)
# │ ┌───────────── hour (0 - 23)
# │ │ ┌───────────── day of the month (1 - 31)
# │ │ │ ┌───────────── month (1 - 12)
# │ │ │ │ ┌───────────── day of the week (0 - 6) (Sunday to Saturday;
# │ │ │ │ │                                   7 is also Sunday on some systems)
# │ │ │ │ │                                   OR sun, mon, tue, wed, thu, fri, sat
# │ │ │ │ │
# * * * * *
```

#### Duration
Duration allows compound durations with minutes and hours values such as `10h5m` or `30m` or `160h`. Since cron syntax does not accept denominations smaller than minutes, users can only define minutes or hours.

{{% alert title="Note" color="primary" %}}
Duration and Schedule must be defined together. When omitted, the budget is always active. When defined, the schedule determines a starting point where the budget will begin being enforced, and the duration determines how long from that starting point the budget will be enforced.
{{% /alert %}}

### Pod-Level Controls

Pods with blocking PDBs will not be evicted by the [Termination Controller]({{<ref "#termination-controller">}}) or be considered for voluntary disruption actions. When multiple pods on a node have different PDBs, none of the PDBs may be blocking for Karpenter to voluntary disrupt a node. This can create complex eviction scenarios:
  - If a pod matches multiple PDBs (via label selectors), ALL of these PDBs must allow for disruption
  - When different pods on the same node belong to different PDBs, ALL PDBs must simultaneously permit eviction
  - A single blocking PDB can prevent the entire node from being voluntary disrupted

For example, consider a node with these pods and PDBs:
- Pod A: Matches PDB-1 (maxUnavailable: 0) and PDB-2 (maxUnavailable: 1)
- Pod B: Matches PDB-3 (minAvailable: 100%)
- Pod C: No PDB

In this scenario, Karpenter cannot voluntary disrupt the node because:
1. Pod A is blocked by PDB-1 even though PDB-2 would allow disruption
2. Pod B is blocked by PDB-3's requirement for 100% availability

As seen in this example, the more PDBs there are affecting a Node, the more difficult it will be for Karpenter to find an opportunity to perform voluntary disruption actions.

Secondly, you can block Karpenter from voluntarily disrupting and draining pods by adding the `karpenter.sh/do-not-disrupt: "true"` annotation to the pod.
You can treat this annotation as a single-pod, permanently blocking PDB.
This has the following consequences:
- Nodes with `karpenter.sh/do-not-disrupt` pods will be excluded from [Consolidation]({{<ref "#consolidation" >}}), and conditionally excluded from [Drift]({{<ref "#drift" >}}).
  - If the Node's owning NodeClaim has a [`terminationGracePeriod`]({{<ref "#terminationgraceperiod" >}}) configured, it will still be eligible for disruption via drift.
- Like pods with a blocking PDB, pods with the `karpenter.sh/do-not-disrupt` annotation will **not** be gracefully evicted by the [Termination Controller]({{<ref "#termination-controller">}}).
  Karpenter will not be able to complete termination of the node until one of the following conditions is met:
  - All pods with the `karpenter.sh/do-not-disrupt` annotation are removed.
  - All pods with the `karpenter.sh/do-not-disrupt` annotation have entered a [terminal phase](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-phase) (`Succeeded` or `Failed`).
  - The owning NodeClaim's [`terminationGracePeriod`]({{<ref "#terminationgraceperiod" >}}) has elapsed.

This is useful for pods that you want to run from start to finish without disruption.
Examples of pods that you might want to opt-out of disruption include an interactive game that you don't want to interrupt or a long batch job (such as you might have with machine learning) that would need to start over if it were interrupted.

```yaml
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    metadata:
      annotations:
        karpenter.sh/do-not-disrupt: "true"
```

{{% alert title="Note" color="primary" %}}
The `karpenter.sh/do-not-disrupt` annotation does **not** exclude nodes from the forceful disruption methods: [Expiration]({{<ref "#expiration" >}}), [Interruption]({{<ref "#interruption" >}}), [Node Repair](<ref "#node-repair" >), and manual deletion (e.g. `kubectl delete node ...`).
While both interruption and node repair have implicit upper-bounds on termination time, expiration and manual termination do not.
Manual intervention may be required to unblock node termination, by removing pods with the `karpenter.sh/do-not-disrupt` annotation.
For this reason, it is not recommended to use the `karpenter.sh/do-not-disrupt` annotation with `expireAfter` **if** you have not also configured `terminationGracePeriod`.
{{% /alert %}}

### Node-Level Controls

You can block Karpenter from voluntarily choosing to disrupt certain nodes by setting the `karpenter.sh/do-not-disrupt: "true"` annotation on the node.
This will prevent voluntary disruption actions against the node.

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    karpenter.sh/do-not-disrupt: "true"
```

#### Example: Disable Disruption on a NodePool

To disable disruption for all nodes launched by a NodePool, you can configure its `.spec.disruption.budgets`. Setting a budget of zero nodes will prevent any of those nodes from being considered for voluntary disruption.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    budgets:
      - nodes: "0"
```
