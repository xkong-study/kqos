package agent

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kongxiangrui/kqos/pkg/agent/collector"
	"github.com/kongxiangrui/kqos/pkg/agent/sysadvisor"
	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/metrics"
	"github.com/kongxiangrui/kqos/pkg/qos"
	"github.com/kongxiangrui/kqos/pkg/usage"
)

// report writes the agent's findings into the node's NodeResourceProfile,
// creating the object on first run.
//
// The profile is owned by the Node, so when a node leaves the cluster the API
// server garbage-collects its profile. Without that owner reference a churny
// cluster accumulates one dead profile per node it ever had, and the
// controller's rollup slowly drifts away from reality.
func (a *Agent) report(ctx context.Context, node *corev1.Node, sample collector.NodeSample, rec sysadvisor.Recommendation) error {
	profile := &v1alpha1.NodeResourceProfile{}
	err := a.client.Get(ctx, types.NamespacedName{Name: a.opts.NodeName}, profile)

	if apierrors.IsNotFound(err) {
		profile = &v1alpha1.NodeResourceProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name: a.opts.NodeName,
				Labels: map[string]string{
					v1alpha1.LabelManaged: "true",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1",
					Kind:       "Node",
					Name:       node.Name,
					UID:        node.UID,
				}},
			},
			Spec: v1alpha1.NodeResourceProfileSpec{NodeName: a.opts.NodeName},
		}
		if err := a.client.Create(ctx, profile); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := a.client.Get(ctx, types.NamespacedName{Name: a.opts.NodeName}, profile); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	base := profile.DeepCopy()
	profile.Status = a.buildStatus(node, sample, rec, profile.Status.Conditions)

	// A status-only merge patch keeps the agent from ever racing the controller
	// over spec fields, and costs one API write per tick instead of a
	// read-modify-write conflict loop.
	return a.client.Status().Patch(ctx, profile, client.MergeFrom(base))
}

func (a *Agent) buildStatus(
	node *corev1.Node,
	sample collector.NodeSample,
	rec sysadvisor.Recommendation,
	existing []metav1.Condition,
) v1alpha1.NodeResourceProfileStatus {
	allocations := make(map[v1alpha1.QoSLevel]v1alpha1.QoSAllocation)
	for level, totals := range sample.ByQoS() {
		allocations[level] = v1alpha1.QoSAllocation{
			Pods:      totals.Pods,
			Requested: totals.ToResourceList("requested"),
			Limits:    totals.ToResourceList("limits"),
			Actual:    totals.ToResourceList("actual"),
		}
	}

	status := v1alpha1.NodeResourceProfileStatus{
		Capacity:        node.Status.Capacity,
		Allocatable:     node.Status.Allocatable,
		QoSAllocations:  allocations,
		Reclaimable:     rec.ReclaimableResourceList(),
		Pressure:        rec.Pressure,
		CPUSetPools:     a.pools.StatusPools(),
		TopologyZones:   a.pools.StatusZones(),
		AdvisorRevision: rec.Revision,
		LastReportTime:  metav1.NewTime(sample.Timestamp),
		Conditions:      existing,
	}

	setCondition(&status.Conditions, metav1.Condition{
		Type:    "AgentHealthy",
		Status:  metav1.ConditionTrue,
		Reason:  "Sampling",
		Message: "agent is sampling cgroup v2 and reporting normally",
	})
	if sample.Degraded {
		setCondition(&status.Conditions, metav1.Condition{
			Type:    "AgentHealthy",
			Status:  metav1.ConditionFalse,
			Reason:  "DegradedSampling",
			Message: "cgroup hierarchy unreadable; figures are estimates and eviction is suspended",
		})
	}
	setCondition(&status.Conditions, metav1.Condition{
		Type:    "RecommendationReady",
		Status:  boolCondition(rec.Confidence != "Low"),
		Reason:  "Confidence" + rec.Confidence,
		Message: "reclaimable capacity bound by " + rec.Reason,
	})
	return status
}

// setCondition upserts a condition, preserving LastTransitionTime when the
// status has not changed so that "how long has this been broken?" stays
// answerable.
func setCondition(conditions *[]metav1.Condition, cond metav1.Condition) {
	cond.ObservedGeneration = 0
	now := metav1.NewTime(time.Now())
	for i := range *conditions {
		if (*conditions)[i].Type != cond.Type {
			continue
		}
		if (*conditions)[i].Status == cond.Status {
			cond.LastTransitionTime = (*conditions)[i].LastTransitionTime
		} else {
			cond.LastTransitionTime = now
		}
		(*conditions)[i] = cond
		return
	}
	cond.LastTransitionTime = now
	*conditions = append(*conditions, cond)
}

func boolCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// exportMetrics publishes the tick's findings to Prometheus.
func (a *Agent) exportMetrics(sample collector.NodeSample, rec sysadvisor.Recommendation) {
	node := a.opts.NodeName

	metrics.NodeReclaimable.WithLabelValues(node, "cpu").Set(float64(rec.ReclaimableCPUMilli))
	metrics.NodeReclaimable.WithLabelValues(node, "memory").Set(float64(rec.ReclaimableMemoryBytes))
	metrics.NodeProtectedUsage.WithLabelValues(node, "cpu").Set(float64(rec.ProtectedCPUMilli))
	metrics.NodeProtectedUsage.WithLabelValues(node, "memory").Set(float64(rec.ProtectedMemoryBytes))
	metrics.NodePressureLevel.WithLabelValues(node).Set(metrics.PressureValue(string(rec.Pressure.Level)))
	metrics.NodePressureStall.WithLabelValues(node, "cpu", "some").Set(float64(rec.Pressure.CPUSomeStalledPercent))
	metrics.NodePressureStall.WithLabelValues(node, "memory", "full").Set(float64(rec.Pressure.MemoryFullStalledPercent))

	byQoS := sample.ByQoS()
	// Reset every known level rather than only the ones present, so a class
	// that drops to zero pods reports zero instead of keeping its last value
	// forever.
	for _, level := range v1alpha1.KnownQoSLevels {
		t := byQoS[level]
		l := string(level)
		metrics.QoSPods.WithLabelValues(node, l).Set(float64(t.Pods))
		metrics.QoSUsage.WithLabelValues(node, l, "cpu").Set(t.ActualCPUMilli)
		metrics.QoSUsage.WithLabelValues(node, l, "memory").Set(float64(t.ActualMemoryBytes))
		metrics.QoSRequested.WithLabelValues(node, l, "cpu").Set(float64(t.RequestedCPUMilli))
		metrics.QoSRequested.WithLabelValues(node, l, "memory").Set(float64(t.RequestedMemoryBytes))
	}
}

// buildReport packages the tick's per-pod samples for the controller's
// profiling store.
func buildReport(nodeName string, sample collector.NodeSample, pods []*corev1.Pod) usage.Report {
	byUID := make(map[string]*corev1.Pod, len(pods))
	for _, p := range pods {
		byUID[string(p.UID)] = p
	}

	report := usage.Report{
		Node:      nodeName,
		Timestamp: sample.Timestamp,
		Degraded:  sample.Degraded,
		Pods:      make([]usage.PodUsage, 0, len(sample.Pods)),
	}
	for _, ps := range sample.Pods {
		if !ps.Valid {
			continue
		}
		u := usage.PodUsage{
			UID:                ps.UID,
			Namespace:          ps.Namespace,
			Name:               ps.Name,
			QoSLevel:           string(ps.QoSLevel),
			CPUMilli:           ps.CPUMilli,
			MemoryBytes:        ps.MemoryBytes,
			ThrottledRatio:     ps.ThrottledRatio,
			RequestCPUMilli:    ps.RequestedCPUMilli(),
			RequestMemoryBytes: ps.RequestedMemoryBytes(),
		}
		if pod, ok := byUID[ps.UID]; ok {
			if owner := metav1.GetControllerOf(pod); owner != nil {
				u.OwnerKind, u.OwnerName = owner.Kind, owner.Name
			}
			// Reclaimed pods carry their footprint in extended resources, so
			// the native requests are zero and would make the workload look
			// infinitely wasteful.
			if eff := qos.EffectiveRequests(pod); u.RequestCPUMilli == 0 {
				if q, ok := eff[corev1.ResourceCPU]; ok {
					u.RequestCPUMilli = q.MilliValue()
				}
				if q, ok := eff[corev1.ResourceMemory]; ok {
					u.RequestMemoryBytes = q.Value()
				}
			}
		}
		report.Pods = append(report.Pods, u)
	}
	return report
}
