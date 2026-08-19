// Package profile turns the raw usage stream into per-workload statistics and
// a request recommendation.
//
// This is the component that answers "why does the reclaimed tier have
// anything to sell?". The reclaimable capacity the advisor computes is real
// precisely because workloads request far more than they use, and a
// WorkloadProfile names the workloads responsible so the waste can be fixed at
// source rather than only worked around.
package profile

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/xkong-study/kqos/pkg/metrics"
	"github.com/xkong-study/kqos/pkg/usage"
)

// Reconciler fills in WorkloadProfile status from the usage store.
type Reconciler struct {
	client.Client
	Store *usage.Store
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadprofile").
		For(&v1alpha1.WorkloadProfile{}).
		Complete(r)
}

// Reconcile recomputes one workload's statistics.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	wp := &v1alpha1.WorkloadProfile{}
	if err := r.Get(ctx, req.NamespacedName, wp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	key := usage.WorkloadKey{
		Namespace: wp.Namespace,
		Kind:      wp.Spec.TargetRef.Kind,
		Name:      wp.Spec.TargetRef.Name,
	}
	cpuStats, memStats, pods, ok := r.Store.WorkloadStats(key)

	base := wp.DeepCopy()
	if !ok {
		wp.Status.Confidence = "Low"
		setCondition(&wp.Status.Conditions, metav1.Condition{
			Type:    "ProfileReady",
			Status:  metav1.ConditionFalse,
			Reason:  "NoSamples",
			Message: "no usage samples yet for " + key.String(),
		})
		wp.Status.LastUpdated = metav1.Now()
		if err := r.Status().Patch(ctx, wp, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	reqCPUMilli, reqMemBytes, _ := r.Store.WorkloadRequests(key)

	margin := float64(wp.Spec.SafetyMarginPercent)
	if margin <= 0 {
		margin = 20
	}
	factor := 1 + margin/100

	// CPU is recommended off P95 and memory off the observed maximum. The
	// asymmetry is not an oversight: exceeding a CPU recommendation costs
	// latency, exceeding a memory recommendation costs the process.
	recCPUMilli := int64(cpuStats.P95 * factor)
	recMemBytes := int64(memStats.Max * factor)

	// Recommendations below a floor are noise -- nobody should be told to set a
	// 3m CPU request -- and a floor also keeps a briefly-idle workload from
	// being recommended into an OOM loop.
	if recCPUMilli < 10 {
		recCPUMilli = 10
	}
	if recMemBytes < 16*1024*1024 {
		recMemBytes = 16 * 1024 * 1024
	}

	waste := int32(0)
	if reqCPUMilli > 0 {
		unused := float64(reqCPUMilli) - cpuStats.P95
		if unused > 0 {
			waste = int32(unused / float64(reqCPUMilli) * 100)
		}
	}

	confidence := "Low"
	switch {
	case cpuStats.Samples >= 120 && pods > 1:
		confidence = "High"
	case cpuStats.Samples >= 30:
		confidence = "Medium"
	}

	wp.Status.Samples = cpuStats.Samples
	wp.Status.PodCount = int32(pods)
	wp.Status.CPU = toUsageStats(cpuStats, milliQuantity)
	wp.Status.Memory = toUsageStats(memStats, byteQuantity)
	wp.Status.CurrentRequests = corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(reqCPUMilli, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(reqMemBytes, resource.BinarySI),
	}
	wp.Status.Recommendation = corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(recCPUMilli, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(recMemBytes, resource.BinarySI),
	}
	wp.Status.WastePercent = waste
	wp.Status.Confidence = confidence
	wp.Status.LastUpdated = metav1.Now()

	setCondition(&wp.Status.Conditions, metav1.Condition{
		Type:   "ProfileReady",
		Status: metav1.ConditionTrue,
		Reason: "Sampled",
		Message: fmt.Sprintf("%d samples across %d pods; p95 cpu %.0fm against a %dm request",
			cpuStats.Samples, pods, cpuStats.P95, reqCPUMilli),
	})

	metrics.WorkloadWastePercent.WithLabelValues(wp.Namespace, wp.Spec.TargetRef.Name).Set(float64(waste))

	if err := r.Status().Patch(ctx, wp, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("profiled workload",
		"workload", key.String(), "samples", cpuStats.Samples,
		"p95CpuMilli", int64(cpuStats.P95), "wastePercent", waste, "confidence", confidence)

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func toUsageStats(s usage.Stats, conv func(float64) resource.Quantity) v1alpha1.UsageStatistics {
	return v1alpha1.UsageStatistics{
		P50: conv(s.P50),
		P90: conv(s.P90),
		P95: conv(s.P95),
		P99: conv(s.P99),
		Max: conv(s.Max),
	}
}

func milliQuantity(v float64) resource.Quantity {
	return *resource.NewMilliQuantity(int64(v), resource.DecimalSI)
}

func byteQuantity(v float64) resource.Quantity {
	return *resource.NewQuantity(int64(v), resource.BinarySI)
}

func setCondition(conditions *[]metav1.Condition, cond metav1.Condition) {
	now := metav1.Now()
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

// NewOwnerResolver builds the resolver the usage store uses to collapse a pod's
// immediate owner into the workload a human recognises.
//
// A pod's controller is a ReplicaSet whose name changes on every rollout. If
// profiles were keyed on that, every deployment would reset its own history at
// exactly the moment the history became interesting, so the resolver walks one
// more hop to the Deployment.
func NewOwnerResolver(c client.Client) usage.OwnerResolver {
	return func(namespace, kind, name string) (usage.WorkloadKey, bool) {
		if kind != "ReplicaSet" {
			return usage.WorkloadKey{Namespace: namespace, Kind: kind, Name: name}, true
		}
		rs := &appsv1.ReplicaSet{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, rs); err != nil {
			return usage.WorkloadKey{Namespace: namespace, Kind: kind, Name: name}, true
		}
		if owner := metav1.GetControllerOf(rs); owner != nil && owner.Kind == "Deployment" {
			return usage.WorkloadKey{Namespace: namespace, Kind: "Deployment", Name: owner.Name}, true
		}
		return usage.WorkloadKey{Namespace: namespace, Kind: kind, Name: name}, true
	}
}
