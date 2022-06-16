<!--
---
linkTitle: "LimitRange"
weight: 300
---
-->

# Compute Resources in Tekton

## Background: Resource Requirements in Kubernetes

Kubernetes allows users to specify CPU, memory, and ephemeral storage constraints
for [containers](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/).
Resource requests determine the resources reserved for a pod when it's scheduled,
and affect likelihood of pod eviction. Resource limits constrain the maximum amount of
a resource a container can use. A container that exceeds its memory limits will be killed,
and a container that exceeds its CPU limits will be throttled.

A pod's [effective resource requests and limits](https://kubernetes.io/docs/concepts/workloads/pods/init-containers/#resources)
are the higher of:
- the sum of all app containers request/limit for a resource
- the effective init container request/limit for a resource

This formula exists because Kubernetes runs init containers sequentially and app containers
in parallel. (There is no distinction made between app containers and sidecar containers
in Kubernetes; a sidecar is used in the following example to illustrate this.)

For example, consider a pod with the following containers:

| Container           | CPU request | CPU limit |
| ------------------- | ----------- | --------- |
| init container 1    | 1           | 2         |
| init container 2    | 2           | 3         |
| app container 1     | 1           | 2         |
| app container 2     | 2           | 3         |
| sidecar container 1 | 3           | no limit  |

The sum of all app container CPU requests is 6 (including the sidecar container), which is
greater than the maximum init container CPU request (2). Therefore, the pod's effective CPU
request will be 6.

Since the sidecar container has no CPU limit, this is treated as the highest CPU limit.
Therefore, the pod will have no effective CPU limit.

## Task Resource Requirements

Tekton allows users to specify resource requirements of [`Steps`](./tasks.md#defining-steps),
which run sequentially. However, the pod's effective resource requirements are still the
sum of its containers' resource requirements. This means that when specifying resource 
requirements for `Step` containers, they must be treated as if they are running in parallel.

Tekton adjusts `Step` resource request (but not limit) requirements to comply with [LimitRanges](#limitrange-support).
[ResourceQuotas](#resourcequota-support) are not currently supported.

## LimitRange Support

Kubernetes allows users to configure [LimitRanges]((https://kubernetes.io/docs/concepts/policy/limit-range/)),
which constrain compute resources of pods, containers, or PVCs running in the same namespace.

LimitRanges can:
- Enforce minimum and maximum compute resources usage per Pod or Container in a namespace.
- Enforce minimum and maximum storage request per PersistentVolumeClaim in a namespace.
- Enforce a ratio between request and limit for a resource in a namespace.
- Set default request/limit for compute resources in a namespace and automatically inject them to Containers at runtime.

Tekton applies the resource requirements specified by users directly to the containers
in a `Task's` pod, unless there is a LimitRange present in the namespace.
(Tekton doesn't allow users to configure init containers for a `Task`.)
Tekton supports LimitRange minimum, maximum, and default resource requirements for containers,
but does not support LimitRange ratios between requests and limits ([#4230](https://github.com/tektoncd/pipeline/issues/4230)).
LimitRange types other than "Container" are not supported.

### Requests

If a `Step` does not have requests defined, the resulting container's requests are the larger of:
- the LimitRange minimum resource requests 
- the LimitRange default resource requests, divided among the other "step" containers

If a `Step` has requests defined, the resulting container's requests are the larger of:
- the `Step's` requests
- the LimitRange minimum resource requests

Any `Sidecar` or other app containers are otherwise not given special treatment and inherit the default LimitRange behavior.

### Limits

All `Step`, `Sidecar`, and other app containers in a TaskRun Pod inherit the default LimitRange behavior.

### Examples

Consider the following LimitRange:

```
apiVersion: v1
kind: LimitRange
metadata:
  name: limitrange-example
spec:
  limits:
  - default:  # The default limits
      cpu: 2
    defaultRequest:  # The default requests
      cpu: 1
    max:  # The maximum limits
      cpu: 3
    min:  # The minimum requests
      cpu: 300m
    type: Container
```

A `Task` with 2 `Steps` and no resources specified would result in a pod with the following containers:

| Container    | CPU request | CPU limit |
| ------------ | ----------- | --------- |
| container 1  | 500m        | 2         |
| container 2  | 500m        | 2         |

Here, the default CPU request was divided among app containers, and this value was used since it was greater
than the minimum request specified by the LimitRange.
The CPU limits are 2 for each container, as this is the default limit specifed in the LimitRange.

Now, consider a `Task` with the following `Step`s:

| Step   | CPU request | CPU limit |
| ------ | ----------- | --------- |
| step 1 | 200m        | 2         |
| step 2 | 1           | 3         |

The resulting pod would have the following containers:

| Container    | CPU request | CPU limit |
| ------------ | ----------- | --------- |
| container 1  | 300m        | 2         |
| container 2  | 1           | 3         |

Here, the first `Step's` request was less than the LimitRange minimum, so the output request is the minimum (300m).
The second `Step's` request is unchanged. The first `Step's` limit is less than the maximum, so it is unchanged,
while the second `Step's` limit is equal to the maximum, so the limit is valid.

Finally, consider a `Task` with the following `Step`s:

| Step   | CPU request | CPU limit |
| ------ | ----------- | --------- |
| step 1 | 200m        | 2         |
| step 2 | 1           | 5         |

The resulting pod would generate an error as it defines an unacceptable container as the CPU limit is too large.

`Error from server (Forbidden): error when creating ".../cpu-constraints-task-pod.yaml":
pods "task-constraints-cpu-demo" is forbidden: maximum cpu usage per Container is 5, but limit is 3.`

### Support for multiple LimitRanges

Tekton supports running `TaskRuns` in namespaces with multiple LimitRanges.
For a given resource, the minumum used will be the largest of any of the LimitRanges' minimum values,
and the maximum used will be the smallest of any of the LimitRanges' maximum values.

The minimum resource requirement used will be the largest of any minimum for that resource,
and the maximum resource requirement will be the smallest of any of the maximum values defined.
The default value will be the minimum of any default values defined.
If the resulting default value is less than the resulting minimum value, the default value will be the minimum value.

It's possible for multiple LimitRanges to be defined which are not compatible with each other, preventing pods from being scheduled.

#### Example

Consider a namespaces with the following LimitRanges defined:

```
apiVersion: v1
kind: LimitRange
metadata:
  name: limitrange-1
spec:
  limits:
  - default:  # The default limits
      cpu: 2
    defaultRequest:  # The default requests
      cpu: 750m
    max:  # The maximum limits
      cpu: 3
    min:  # The minimum requests
      cpu: 500m
    type: Container
```

```
apiVersion: v1
kind: LimitRange
metadata:
  name: limitrange-2
spec:
  limits:
  - default:  # The default limits
      cpu: 1.5
    defaultRequest:  # The default requests
      cpu: 1
    max:  # The maximum limits
      cpu: 2.5
    min:  # The minimum requests
      cpu: 300m
    type: Container
```

A namespace with limitrange-1 and limitrange-2 would be treated as if it contained only the following LimitRange:

```
apiVersion: v1
kind: LimitRange
metadata:
  name: aggregate-limitrange
spec:
  limits:
  - default:  # The default limits
      cpu: 1.5
    defaultRequest:  # The default requests
      cpu: 750m
    max:  # The maximum limits
      cpu: 2.5
    min:  # The minimum requests
      cpu: 300m
    type: Container
```

Here, the minimum of the "max" values is the output "max" value, and likewise for "default" and "defaultRequest".
The maximum of the "min" values is the output "min" value.

## ResourceQuota Support

Kubernetes allows users to define [ResourceQuotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/),
which restrict the maximum resource requests and limits of all pods running in a namespace.
`TaskRuns` can't currently be created in a namespace with ResourceQuotas without siginificant caveats
([#2933](https://github.com/tektoncd/pipeline/issues/2933)).

# References

- [LimitRange in k8s docs](https://kubernetes.io/docs/concepts/policy/limit-range/)
- [Configure default memory requests and limits for a Namespace](https://kubernetes.io/docs/tasks/administer-cluster/manage-resources/memory-default-namespace/)
- [Configure default CPU requests and limits for a Namespace](https://kubernetes.io/docs/tasks/administer-cluster/manage-resources/cpu-default-namespace/)
- [Configure Minimum and Maximum CPU constraints for a Namespace](https://kubernetes.io/docs/tasks/administer-cluster/manage-resources/cpu-constraint-namespace/)
- [Configure Minimum and Maximum Memory constraints for a Namespace](https://kubernetes.io/docs/tasks/administer-cluster/manage-resources/memory-constraint-namespace/)
- [Managing Resources for Containers](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/)
- [Kubernetes best practices: Resource requests and limits](https://cloud.google.com/blog/products/containers-kubernetes/kubernetes-best-practices-resource-requests-and-limits)
- [Restrict resource consumption with limit ranges](https://docs.openshift.com/container-platform/4.8/nodes/clusters/nodes-cluster-limit-ranges.html)
