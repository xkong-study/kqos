package profile

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
)

// DiscoveryReconciler creates a WorkloadProfile for every Deployment it sees.
//
// Profiling only helps if it is on by default. Requiring a team to opt in
// means the workloads that waste the most -- the ones nobody has looked at in
// two years -- are exactly the ones that never get a profile.
type DiscoveryReconciler struct {
	client.Client

	// SkipNamespaces are left unprofiled, normally the cluster's own system
	// namespaces where a resource recommendation is not actionable anyway.
	SkipNamespaces []string
}

// SetupWithManager registers the reconciler.
func (r *DiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloaddiscovery").
		For(&appsv1.Deployment{}).
		Owns(&v1alpha1.WorkloadProfile{}).
		Complete(r)
}

// Reconcile ensures a profile exists for one Deployment.
func (r *DiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	for _, ns := range r.SkipNamespaces {
		if req.Namespace == ns {
			return ctrl.Result{}, nil
		}
	}

	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deploy); err != nil {
		if apierrors.IsNotFound(err) {
			// The profile is owned by the Deployment, so deletion cascades and
			// there is nothing to clean up here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	wp := &v1alpha1.WorkloadProfile{}
	err := r.Get(ctx, req.NamespacedName, wp)
	if err == nil {
		return ctrl.Result{}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	level := v1alpha1.QoSLevel(deploy.Spec.Template.Annotations[v1alpha1.AnnotationQoSLevel])
	if !level.IsValid() {
		level = v1alpha1.QoSSharedCores
	}

	wp = &v1alpha1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
			Labels:    map[string]string{v1alpha1.LabelManaged: "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploy.Name,
				UID:        deploy.UID,
			}},
		},
		Spec: v1alpha1.WorkloadProfileSpec{
			TargetRef: v1alpha1.TargetReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploy.Name,
			},
			QoSLevel:            level,
			SafetyMarginPercent: 20,
		},
	}
	if err := r.Create(ctx, wp); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	logger.Info("created workload profile", "workload", req.String(), "qosLevel", level)
	return ctrl.Result{}, nil
}
