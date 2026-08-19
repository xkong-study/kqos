package overcommit

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
)

// reclaimableChanged filters the agent's status writes down to the ones that
// can change a node's advertisement.
//
// Agents patch their profile every ten seconds, and almost every patch only
// moves LastReportTime. Reconciling on all of them would turn a 200-node
// cluster into 20 pointless node-status patches per second. The advisor
// already suppresses insignificant movement behind AdvisorRevision, so that
// counter is the right thing to watch.
func reclaimableChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldP, okOld := e.ObjectOld.(*v1alpha1.NodeResourceProfile)
			newP, okNew := e.ObjectNew.(*v1alpha1.NodeResourceProfile)
			if !okOld || !okNew {
				return true
			}
			if oldP.Status.AdvisorRevision != newP.Status.AdvisorRevision {
				return true
			}
			if oldP.Status.Pressure.Level != newP.Status.Pressure.Level {
				return true
			}
			// The health condition flipping matters even when the numbers have
			// not moved: an agent that has just gone degraded should stop being
			// trusted immediately.
			return conditionStatus(oldP, "AgentHealthy") != conditionStatus(newP, "AgentHealthy")
		},
	}
}

func conditionStatus(p *v1alpha1.NodeResourceProfile, condType string) string {
	for _, c := range p.Status.Conditions {
		if c.Type == condType {
			return string(c.Status)
		}
	}
	return ""
}
