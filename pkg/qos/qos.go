// Package qos holds the helpers that read a pod's kqos classification and
// resource footprint. It is imported by the agent, the controller, the webhook
// and the scheduler plugin, so it deliberately depends on nothing but the
// Kubernetes core types.
package qos

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
)

// LevelOf returns the pod's declared kqos level.
//
// When no annotation is present the level is inferred from the pod's native
// Kubernetes QoS class. That inference is what makes kqos safe to install on a
// running cluster: every pre-existing pod lands in a sensible class without
// anyone having to relabel it, and BestEffort pods -- which by definition have
// made no promises -- become the initial reclaimed tier.
func LevelOf(pod *corev1.Pod) v1alpha1.QoSLevel {
	if pod == nil {
		return v1alpha1.QoSSharedCores
	}
	if v, ok := pod.Annotations[v1alpha1.AnnotationQoSLevel]; ok {
		if lvl := v1alpha1.QoSLevel(v); lvl.IsValid() {
			return lvl
		}
	}
	// Pods in the kube-system-style critical tier keep the node alive and must
	// never be classified as evictable.
	if pod.Spec.PriorityClassName == "system-node-critical" ||
		pod.Spec.PriorityClassName == "system-cluster-critical" {
		return v1alpha1.QoSSystemCores
	}
	switch nativeQoS(pod) {
	case corev1.PodQOSGuaranteed:
		return v1alpha1.QoSDedicatedCores
	case corev1.PodQOSBestEffort:
		return v1alpha1.QoSReclaimedCores
	default:
		return v1alpha1.QoSSharedCores
	}
}

// nativeQoS recomputes the Kubernetes QoS class rather than trusting
// status.qosClass, which is empty on pods that have not been admitted yet --
// exactly the pods the webhook sees.
func nativeQoS(pod *corev1.Pod) corev1.PodQOSClass {
	if pod.Status.QOSClass != "" {
		return pod.Status.QOSClass
	}
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	isGuaranteed := true
	anyRequest := false

	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			req, hasReq := c.Resources.Requests[name]
			lim, hasLim := c.Resources.Limits[name]
			if hasReq && !req.IsZero() {
				anyRequest = true
				add(requests, name, req)
			}
			if hasLim && !lim.IsZero() {
				add(limits, name, lim)
			}
			if !hasLim || lim.IsZero() {
				isGuaranteed = false
			} else if hasReq && req.Cmp(lim) != 0 {
				isGuaranteed = false
			}
		}
	}
	if !anyRequest && len(limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	if isGuaranteed {
		return corev1.PodQOSGuaranteed
	}
	return corev1.PodQOSBurstable
}

// IsReclaimed reports whether the pod runs on oversold capacity.
func IsReclaimed(pod *corev1.Pod) bool {
	return LevelOf(pod) == v1alpha1.QoSReclaimedCores
}

// WantsNUMABinding reports whether a dedicated pod asked to be confined to one
// NUMA zone. Meaningless for other levels and ignored there.
func WantsNUMABinding(pod *corev1.Pod) bool {
	return pod.Annotations[v1alpha1.AnnotationNUMABinding] == "true" &&
		LevelOf(pod) == v1alpha1.QoSDedicatedCores
}

// CPUSetPoolOf returns the shared pool name a pod belongs to, defaulting to
// "shared".
func CPUSetPoolOf(pod *corev1.Pod) string {
	if v := pod.Annotations[v1alpha1.AnnotationCPUSetPool]; v != "" {
		return v
	}
	return "shared"
}

// PodRequests sums container requests. Init containers are handled the way the
// scheduler does: the pod needs the larger of (max init request) and (sum of
// app requests), because init containers run to completion before the app
// containers start.
func PodRequests(pod *corev1.Pod) corev1.ResourceList {
	return sumPod(pod, func(c corev1.Container) corev1.ResourceList { return c.Resources.Requests })
}

// PodLimits sums container limits with the same init-container rule.
func PodLimits(pod *corev1.Pod) corev1.ResourceList {
	return sumPod(pod, func(c corev1.Container) corev1.ResourceList { return c.Resources.Limits })
}

func sumPod(pod *corev1.Pod, pick func(corev1.Container) corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	if pod == nil {
		return out
	}
	for _, c := range pod.Spec.Containers {
		for name, q := range pick(c) {
			add(out, name, q)
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for name, q := range pick(c) {
			if cur, ok := out[name]; !ok || q.Cmp(cur) > 0 {
				out[name] = q.DeepCopy()
			}
		}
	}
	return out
}

// EffectiveRequests returns the pod's cpu/memory footprint with reclaimed-tier
// extended resources translated back into ordinary units.
//
// A reclaimed pod that the webhook has rewritten carries no cpu or memory
// request at all -- that is the whole point, it must be invisible to the native
// scheduler's capacity maths. But the agent still has to know how big it is in
// order to size pools and pick eviction victims, so it reads the extended
// resources back out.
func EffectiveRequests(pod *corev1.Pod) corev1.ResourceList {
	out := PodRequests(pod)
	if _, ok := out[corev1.ResourceCPU]; !ok {
		if milli := extendedTotal(pod, v1alpha1.ResourceReclaimedCPU); milli > 0 {
			out[corev1.ResourceCPU] = *resource.NewMilliQuantity(milli, resource.DecimalSI)
		}
	}
	if _, ok := out[corev1.ResourceMemory]; !ok {
		if mib := extendedTotal(pod, v1alpha1.ResourceReclaimedMemory); mib > 0 {
			out[corev1.ResourceMemory] = *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
		}
	}
	return out
}

// extendedTotal sums one extended resource across a pod's containers.
func extendedTotal(pod *corev1.Pod, name string) int64 {
	var total int64
	rn := corev1.ResourceName(name)
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Requests[rn]; ok {
			total += q.Value()
		}
	}
	return total
}

// IsTerminal reports whether a pod has stopped consuming resources.
func IsTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// IsMirrorPod reports whether the pod is a static pod managed by the kubelet.
// Evicting one is pointless: the kubelet recreates it immediately.
func IsMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

func add(list corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	if cur, ok := list[name]; ok {
		cur.Add(q)
		list[name] = cur
		return
	}
	list[name] = q.DeepCopy()
}
