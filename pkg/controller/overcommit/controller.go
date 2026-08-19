// Package overcommit turns each node's reclaimable capacity into a resource
// the Kubernetes scheduler can actually place pods against.
//
// The design decision that everything else follows from: reclaimed capacity is
// advertised as an *extended resource* (kqos.io/reclaimed-cpu) rather than by
// inflating the node's real cpu and memory.
//
// Inflating cpu/memory would be simpler and is what naive overcommit does. It
// is also unrecoverable, because it lies to every other component at once --
// the scheduler's fit predicates, the kubelet's admission checks, the
// Horizontal Pod Autoscaler's utilisation maths and every dashboard in the
// company all start working from a capacity that does not exist. Using a
// separate resource dimension keeps the real numbers real: guaranteed
// workloads are still scheduled against genuine capacity, and only pods that
// explicitly opted into the reclaimed tier can consume the invented kind.
package overcommit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/xkong-study/kqos/pkg/metrics"
)

// staleAfter is how long a NodeResourceProfile may go unrefreshed before the
// controller stops trusting it.
//
// A dead agent is the most dangerous failure mode in the system: its last
// report says "this node has 4 spare cores", the scheduler keeps packing
// reclaimed pods onto a node nobody is watching, and nothing evicts them when
// the node fills up. So a stale profile withdraws the advertisement entirely.
const staleAfter = 90 * time.Second

// Reconciler syncs NodeResourceProfile.status.reclaimable onto the Node's
// extended resources.
type Reconciler struct {
	client.Client

	// MinChangeMilli suppresses patches for trivial CPU movements, so a node
	// whose reclaimable figure wobbles by a few millicores does not generate an
	// API write every reconcile.
	MinChangeMilli int64

	// MinChangeBytes is the same suppression for memory.
	MinChangeBytes int64
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.MinChangeMilli == 0 {
		r.MinChangeMilli = 50
	}
	if r.MinChangeBytes == 0 {
		r.MinChangeBytes = 64 * 1024 * 1024
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("overcommit").
		For(&v1alpha1.NodeResourceProfile{}, builder.WithPredicates(reclaimableChanged())).
		Complete(r)
}

// Reconcile brings one node's extended resources in line with its profile.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", req.Name)

	profile := &v1alpha1.NodeResourceProfile{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			// The profile is gone, which means the node is gone too (the
			// profile is owned by it). Nothing to clean up.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: profile.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cpuMilli, memBytes := desiredFrom(profile)

	if stale(profile) {
		logger.Info("profile is stale; withdrawing reclaimed capacity",
			"lastReport", profile.Status.LastReportTime.Time,
			"staleAfter", staleAfter)
		cpuMilli, memBytes = 0, 0
	}

	changed, err := r.sync(ctx, node, cpuMilli, memBytes)
	if err != nil {
		metrics.NodeExtendedResourceSyncs.WithLabelValues("error").Inc()
		return ctrl.Result{}, err
	}
	if changed {
		metrics.NodeExtendedResourceSyncs.WithLabelValues("updated").Inc()
		logger.Info("advertised reclaimed capacity",
			"reclaimedCpuMilli", cpuMilli,
			"reclaimedMemoryMiB", memBytes/(1024*1024),
			"pressure", profile.Status.Pressure.Level)
	} else {
		metrics.NodeExtendedResourceSyncs.WithLabelValues("nochange").Inc()
	}

	// Requeue so a node whose agent dies has its advertisement withdrawn even
	// though no watch event will ever fire for it.
	return ctrl.Result{RequeueAfter: staleAfter / 2}, nil
}

// desiredFrom extracts the advertised amounts from a profile, in the integer
// units extended resources require: millicores for CPU, mebibytes for memory.
func desiredFrom(p *v1alpha1.NodeResourceProfile) (cpuMilli, memBytes int64) {
	if q, ok := p.Status.Reclaimable[corev1.ResourceCPU]; ok {
		cpuMilli = q.MilliValue()
	}
	if q, ok := p.Status.Reclaimable[corev1.ResourceMemory]; ok {
		memBytes = q.Value()
	}
	if cpuMilli < 0 {
		cpuMilli = 0
	}
	if memBytes < 0 {
		memBytes = 0
	}
	return cpuMilli, memBytes
}

func stale(p *v1alpha1.NodeResourceProfile) bool {
	if p.Status.LastReportTime.IsZero() {
		return true
	}
	return time.Since(p.Status.LastReportTime.Time) > staleAfter
}

// sync patches the node's status if the desired values differ enough to be
// worth a write.
func (r *Reconciler) sync(ctx context.Context, node *corev1.Node, cpuMilli, memBytes int64) (bool, error) {
	memMiB := memBytes / (1024 * 1024)

	curCPU := extendedValue(node, v1alpha1.ResourceReclaimedCPU)
	curMem := extendedValue(node, v1alpha1.ResourceReclaimedMemory)

	// Any move to or from zero is always applied: withdrawing capacity is a
	// safety action and must never be suppressed as "too small a change".
	significant := (cpuMilli == 0) != (curCPU == 0) ||
		(memMiB == 0) != (curMem == 0) ||
		abs(cpuMilli-curCPU) >= r.MinChangeMilli ||
		abs(memMiB-curMem)*1024*1024 >= r.MinChangeBytes

	if !significant {
		return false, nil
	}

	patch := map[string]any{
		"status": map[string]any{
			"capacity": map[string]any{
				v1alpha1.ResourceReclaimedCPU:    fmt.Sprint(cpuMilli),
				v1alpha1.ResourceReclaimedMemory: fmt.Sprint(memMiB),
			},
			"allocatable": map[string]any{
				v1alpha1.ResourceReclaimedCPU:    fmt.Sprint(cpuMilli),
				v1alpha1.ResourceReclaimedMemory: fmt.Sprint(memMiB),
			},
		},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}
	// A strategic-merge patch on the node status subresource. Capacity is a
	// map, so this adds or replaces exactly the two kqos keys and leaves the
	// kubelet's cpu, memory and pods entries untouched.
	if err := r.Status().Patch(ctx, node, client.RawPatch(types.StrategicMergePatchType, raw)); err != nil {
		return false, fmt.Errorf("patch node %s status: %w", node.Name, err)
	}
	return true, nil
}

// extendedValue reads an extended resource from the node's allocatable list.
func extendedValue(node *corev1.Node, name string) int64 {
	if q, ok := node.Status.Allocatable[corev1.ResourceName(name)]; ok {
		return q.Value()
	}
	return 0
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// AdvertisedFor renders a node's current advertisement, used by the policy
// rollup and by tests.
func AdvertisedFor(node *corev1.Node) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU: *resource.NewMilliQuantity(
			extendedValue(node, v1alpha1.ResourceReclaimedCPU), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(
			extendedValue(node, v1alpha1.ResourceReclaimedMemory)*1024*1024, resource.BinarySI),
	}
}
