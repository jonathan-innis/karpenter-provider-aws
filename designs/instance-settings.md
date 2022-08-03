# User-Defined Instance Settings and Kubelet Overrides

## Goals

- Allow users to specify `maxPods` overrides to the kubelet per instance type and leverage these for scheduling
- Allow users to specify a VM Memory overhead per instance type
- Allow users to specify generic kubelet configuration arguments per instance type

## Background

Kubelet-specific configuration for nodes deployed by Karpenter is passed through to the `bootstrap.sh` script and is used when bootstrapping the new node to the cluster. Karpenter currently supports certain static default values that are used when bootstrapping the node. In particular, values like `--max-pods=110` are used when the `AWS_ENABLE_POD_ENI` is set to `false`. Users currently have no way to specify extra arguments to the kubelet. Additionally, there is currently no supported way to define per-instance type or per-instance family memory requirements for the `VM_MEMORY_OVERHEAD` value that is leveraged for scheduling decisions.

## Instance Type Setting Examples

1. Configure VM memory overhead and `maxPods` on a specific instance family

```yaml
apiVersion: karpenter.sh/v1alpha5
kind: InstanceTypeSetting
metadata:
  name: c5n-settings
spec:
  weight: 100
  vmReservedMemory: "2Mi" # Can be bytes value or percentage
  maxPods: 20
  selectors:
    - key: karpenter.k8s.aws/instance-family
      values: ["c5n"]
```

2. Configure VM memory overhead as a percentage value for a specific instance type family

```yaml
apiVersion: karpenter.sh/v1alpha5
kind: InstanceTypeSetting
metadata:
  name: c4-settings
spec:
  weight: 100
  vmReservedMemory: "0.75%" # Can be bytes value or percentage
  selectors:
    - key: karpenter.k8s.aws/instance-family
      values: ["c4"]
```

3. Configure a default `kubeletConfiguration` for all instances with `clusterDNS` and `containerRuntime`

```yaml
apiVersion: karpenter.sh/v1alpha5
kind: InstanceTypeSetting
metadata:
  name: deafult-kubelet-settings
spec:
  kubeletConfiguration:
      clusterDNS: ["10.0.1.100"]
      containerRuntime: containerd
  selectors:
    - key: node.kubernetes.io/instance-type
      values: ["*"]
```

4. Configure a `kubeletConfiguration` `extraArguments` override for a specific instance type family grouping

```yaml
apiVersion: karpenter.sh/v1alpha5
kind: InstanceTypeSetting
metadata:
   name: kubelet-settings
spec:
   weight: 50
   kubeletConfiguration:
      extraArguments:
         - "--kube-reserved=cpu=200m,memory=500Mi,ephemeral-storage=1Gi,pid='100'"
         - "--eviction-hard=memory.available<500Mi"
   selectors:
      - key: karpenter.k8s.aws/instance-family
        values: ["c4", "c5"]
```

## Considerations

### Custom Resource Cardinality

One of the primary questions presented is whether we should create a 1-1 mapping between settings and a given instance type or whether we should allow the ability to create more complex structures using set relationships. Options are listed below combined with the pros and cons for each:

**Options**
1. Create an `InstanceType` or `InstanceTypeSetting` CRD that specifies a 1-1 relationship between the settings and the instance type
   1. Pros
        1. Clearly defined relationship between an instance type and the settings that are defined for it
        2. Users that only have a few instance types will no
   2. Cons
      1. Does not scale well if you have to assign settings or configuration to a large number of instances.
      2. I may want to define settings over an instance family without having to individually configure the instances that exist within that family
2. Create an `InstanceTypeSetting` CRD that contains label selectors that allow a 1-many relationship between settings and instance types
   1. Pros
      1. Allows users who want to apply the same instance type settings across a wide range of instance types or across an instance type family to do so
      2. Lower maintenance burden from maintaining less `InstanceTypeSetting` CRDs
   2. Cons
      1. Can create complex relationships that may be difficult to track for users
3. Add instance type settings to the `Provisioner` spec under `.spec.vmMemoryOverhead` and `.spec.maxPods`
   1. Pros
      1. Expands on the existing Provisioner spec without introducing additional concepts to users
      2. Users can leverage existing provisioners to 
   2. Cons
      1. Does not scale well if these settings expand
      2. `Provisioner` CRD becomes a bit monolithic in that it is handling the provisioner based configuration and instance-based configuration. In general, these should not be mixed
4. Add `.spec.instanceSettingRef` to the `Provisioner` spec
   1. This has relatively the same pros and cons as the above solution. It does have some benefits in being slightly more extensible but the concepts of instance-based settings and provisioner-based settings are fairly non-overlapping

**Recommendation:** Use `InstanceTypeSetting` CRD with label selectors to create one-to-many relationships between instances and settings. In general, this will ease the configuration maintenance burden on users.

### Kubelet Configuration Location

Users are allowed to set certain kubelet configuration values through the `Provisioner` spec on a per-provisioner basis. These values do not actually have anything to do with the provisioner-based logic and are generally more tied to generic cluster-wide-based configuration or tied to instance-based configuration.

**Options**:
1. Do not change the existing `kubeletConfiguration` in the `Provisioner` and add `kubeletExtraArguments` within the `InstanceTypeSettings`
   1. Pros
      1. Fits existing expectation around how `kubeletConfiguration` is set for instances
      2. Requires no complex logic for backwards compatability
   2. Cons
      1. Ties configuration that is not specific to the `Provisioner` spec to the `Provisioner` spec
2. Deprecate `kubeletConfiguration` from the `Provisioner` spec and migrate configuration data into `InstanceTypeSettings`
   1. Pros
      1. Allows configuration that may be VM/instance specific to be specified at a separate layer from the provisioner
   2. Cons
      1. Has the potential to create complex relationships between `InstanceTypeSettings` as I may want to specify a common default configuration for all instance types with overrides for specific instance type families
3. Create a `KubeletConfigurationSetting` CRD that contains configuration specific to the kubelet
   1. Pros
      1. Completely separates out the kubelet configuration logic from the instance-type specific configuration
      2. Allows a user to specify consistent kubelet configuration across the entire set of instance types
   2. Cons
      1. Creates another unnecessary abstraction layer that could live within the `InstanceTypeSettings`

**Recommendation:** Add `kubeletConfiguration` into `InstanceTypeSettings` and away from `Provisioner`

### Label Selectors to Allow

We should start with some baseline set of label selectors and then iterate if we later need to expand this set of label selectors. Below are the proposed label selectors to start supporting for `InstanceTypeSetting` CRD

1. `node.kubernetes.io/instance-type`
2. `karpenter.k8s.aws/instance-family`

_Note: We can consider adding other label selectors such as `kubernetes.io/arch` and `kubernetes.io/os` in the future as there are obvious use-cases for these values._

### Mutual Exclusivity of InstanceTypeSettings

With the introduction of `InstanceType` CRD that contains a new rule-set, there is the potential for these rulesets to create unnecessarily complex relationships that overlap each other and provide overrides to each other. Below are a few considerations around this overlap

**Options**
1. Enforce mutual exclusivity between label selectors
   1. Pros
      1. Reduces the scope of the change as it is easy to avoid overlapping and checking overlap between instance types and instance families
      2. Removes the need for providing `priroity` in `InstanceTypeSetting` spec since there can be no overlap between separate settings
   2. Cons
      1. Unnecessarily constrains users who may want to create more complex relationships

2. Allow overlapping `InstanceTypeSettings` and create a `.spec.weight` value. All instance type settings that match a given instance type would be evaluated in weight order (from high to low) to build the instance type settings. As the settings are evaluated, if a specific `.spec.[*]` exists for a higher weight setting, then that `spec.[*]` is prioritized. If two settings exist at the same weight for an instance, a random one is assigned.
   1. Pros
      1. Easy understanding of how to set defaults as a label-selector could be set like the below to imply that this `InstanceTypeSetting` should be attempted to be applied to all instances
      ```yaml
      - key: node.kubernetes.io/instance-type
        values: ["*"]
      ```
   2. Cons
      1. Creates complexity and may eventually lead to confusion around what settings will actually be assigned to a given instance

**Recommendation:** Allow overlapping relationships between instance type settings. This has more sensible results for a defaulting mechanism where a user may want to define configuration across all instance types, then define configuration across all `ARM64` architectures.

## Links/Additional Resources

- [Kubelet Command Line Arguments](https://kubernetes.io/docs/reference/command-line-tools-reference/kubelet/)

- [Kubelet Eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/)