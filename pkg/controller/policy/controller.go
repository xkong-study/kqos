// Package policy maintains the cluster-wide rollup on the QoSPolicy object and
// makes sure a default policy exists.
package policy

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/xkong-study/kqos/pkg/metrics"
)

// agentLivenessWindow is how recently a node must have reported to count as
// ready in the rollup.
const agentLivenessWindow = 90 * time.Second

// Reconciler keeps QoSPolicy.status current.
type Reconciler struct {
	client.Client
}

// SetupWithManager registers the reconciler and makes profile changes wake it,
// since the rollup is a function of every profile in the cluster.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapProfileToPolicy := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{{
				NamespacedName: client.ObjectKey{Name: v1alpha1.DefaultQoSPolicyName},
			}}
		})

	return ctrl.NewControllerManagedBy(mgr).
		Named("qospolicy").
		For(&v1alpha1.QoSPolicy{}).
		Watches(&v1alpha1.NodeResourceProfile{}, mapProfileToPolicy).
		Complete(r)
}

// Reconcile recomputes the rollup.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policy := &v1alpha1.QoSPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	selector := labels.Everything()
	if policy.Spec.NodeSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(policy.Spec.NodeSelector)
		if err != nil {
			return ctrl.Result{}, err
		}
		selector = s
	}

	var profiles v1alpha1.NodeResourceProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return ctrl.Result{}, err
	}

	var (
		observed, ready, underPressure int32
		reclaimCPUMilli, reclaimMem    int64
		allocCPUMilli, allocMem        int64
	)

	for i := range profiles.Items {
		p := &profiles.Items[i]

		// A node selector on the policy is expressed against node labels, and
		// the profile carries the node's name; matching on the profile's own
		// labels would be wrong, so fetch the node when a selector is set.
		if policy.Spec.NodeSelector != nil {
			node := &corev1.Node{}
			if err := r.Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, node); err != nil {
				continue
			}
			if !selector.Matches(labels.Set(node.Labels)) {
				continue
			}
		}

		observed++
		if !p.Status.LastReportTime.IsZero() &&
			time.Since(p.Status.LastReportTime.Time) < agentLivenessWindow {
			ready++
		}
		if p.Status.Pressure.Level != v1alpha1.PressureNone {
			underPressure++
		}
		if q, ok := p.Status.Reclaimable[corev1.ResourceCPU]; ok {
			reclaimCPUMilli += q.MilliValue()
		}
		if q, ok := p.Status.Reclaimable[corev1.ResourceMemory]; ok {
			reclaimMem += q.Value()
		}
		if q, ok := p.Status.Allocatable[corev1.ResourceCPU]; ok {
			allocCPUMilli += q.MilliValue()
		}
		if q, ok := p.Status.Allocatable[corev1.ResourceMemory]; ok {
			allocMem += q.Value()
		}
	}

	var overcommit int32
	if allocCPUMilli > 0 {
		overcommit = int32(reclaimCPUMilli * 100 / allocCPUMilli)
	}

	base := policy.DeepCopy()
	policy.Status = v1alpha1.QoSPolicyStatus{
		ObservedNodes:              observed,
		ReadyNodes:                 ready,
		NodesUnderPressure:         underPressure,
		TotalReclaimableCPU:        *resource.NewMilliQuantity(reclaimCPUMilli, resource.DecimalSI),
		TotalReclaimableMemory:     *resource.NewQuantity(reclaimMem, resource.BinarySI),
		EffectiveOvercommitPercent: overcommit,
		LastUpdated:                metav1.Now(),
		Conditions:                 policy.Status.Conditions,
	}

	metrics.ClusterReclaimable.WithLabelValues("cpu").Set(float64(reclaimCPUMilli))
	metrics.ClusterReclaimable.WithLabelValues("memory").Set(float64(reclaimMem))
	metrics.ClusterOvercommitPercent.Set(float64(overcommit))

	if err := r.Status().Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("policy rollup",
		"nodes", observed, "ready", ready, "underPressure", underPressure,
		"reclaimCpuMilli", reclaimCPUMilli, "overcommitPercent", overcommit)

	// A node whose agent has died produces no events, so the rollup has to
	// re-evaluate on a timer to notice ReadyNodes falling.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// EnsureDefault creates the singleton policy if it is missing, so a fresh
// install is configured rather than relying on every component's built-in
// fallbacks.
func EnsureDefault(ctx context.Context, c client.Client, spec v1alpha1.QoSPolicySpec) error {
	policy := &v1alpha1.QoSPolicy{}
	err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.DefaultQoSPolicyName}, policy)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	policy = &v1alpha1.QoSPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:   v1alpha1.DefaultQoSPolicyName,
			Labels: map[string]string{v1alpha1.LabelManaged: "true"},
		},
		Spec: spec,
	}
	if err := c.Create(ctx, policy); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
