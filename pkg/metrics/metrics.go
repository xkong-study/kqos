// Package metrics defines every Prometheus series kqos exports. Keeping them
// in one package rather than scattered next to their call sites makes the
// contract with dashboards and alerts reviewable in a single file.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "kqos"

var (
	// NodeReclaimable is the headline agent series: how much capacity this node
	// is currently offering to the reclaimed tier.
	NodeReclaimable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "advisor",
		Name:      "reclaimable",
		Help:      "Capacity offered to the reclaimed tier (cpu in millicores, memory in bytes).",
	}, []string{"node", "resource"})

	// NodeProtectedUsage is the smoothed consumption of the tiers kqos
	// guarantees, i.e. the input the reclaimable figure is derived from.
	NodeProtectedUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "advisor",
		Name:      "protected_usage",
		Help:      "Smoothed usage of the system/dedicated/shared tiers (cpu millicores, memory bytes).",
	}, []string{"node", "resource"})

	// QoSUsage breaks measured consumption down by class, which is what makes
	// a utilisation dashboard tell you *who* is using the node rather than just
	// how much of it is used.
	QoSUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "node",
		Name:      "qos_usage",
		Help:      "Measured usage by QoS level (cpu millicores, memory bytes).",
	}, []string{"node", "qos_level", "resource"})

	// QoSRequested is the same breakdown for declared requests. Plotted against
	// QoSUsage it is the waste chart that justifies the whole project.
	QoSRequested = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "node",
		Name:      "qos_requested",
		Help:      "Declared requests by QoS level (cpu millicores, memory bytes).",
	}, []string{"node", "qos_level", "resource"})

	// QoSPods counts pods per class per node.
	QoSPods = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "node",
		Name:      "qos_pods",
		Help:      "Pod count by QoS level.",
	}, []string{"node", "qos_level"})

	// NodePressureLevel is 0 None, 1 Moderate, 2 Critical.
	NodePressureLevel = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "node",
		Name:      "pressure_level",
		Help:      "Node pressure classification: 0=None, 1=Moderate, 2=Critical.",
	}, []string{"node"})

	// NodePressureStall exports the raw PSI inputs behind that classification.
	NodePressureStall = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "node",
		Name:      "pressure_stall_percent",
		Help:      "PSI stall percentage feeding the pressure classification.",
	}, []string{"node", "resource", "kind"})

	// EvictionsTotal counts evictions actually carried out.
	EvictionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "eviction",
		Name:      "total",
		Help:      "Pods evicted by kqos.",
	}, []string{"node", "plugin", "reason", "qos_level"})

	// EvictionsProposedTotal counts verdicts produced by plugins, including
	// those the manager then suppressed. The gap between this and
	// EvictionsTotal is how you tell "the policy is quiet" from "the policy is
	// screaming and the rate limiter is holding it back".
	EvictionsProposedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "eviction",
		Name:      "proposed_total",
		Help:      "Eviction verdicts produced by plugins before safety filtering.",
	}, []string{"node", "plugin", "reason"})

	// EvictionsSuppressedTotal counts verdicts the manager declined to execute.
	EvictionsSuppressedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "eviction",
		Name:      "suppressed_total",
		Help:      "Eviction verdicts suppressed, labelled by the safety rule that stopped them.",
	}, []string{"node", "cause"})

	// EvictionErrorsTotal counts failures from the eviction API, most often a
	// PodDisruptionBudget refusing the request.
	EvictionErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "eviction",
		Name:      "errors_total",
		Help:      "Failed eviction API calls.",
	}, []string{"node", "cause"})

	// CollectDuration times one full sampling pass.
	CollectDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "collect_duration_seconds",
		Help:      "Time to take one full node sample.",
		Buckets:   prometheus.ExponentialBuckets(0.001, 2, 10),
	}, []string{"node"})

	// CollectErrorsTotal counts sampling failures.
	CollectErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "collect_errors_total",
		Help:      "Sampling failures.",
	}, []string{"node", "cause"})

	// ReconcileDuration times the agent's report-and-act loop.
	ReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "agent",
		Name:      "reconcile_duration_seconds",
		Help:      "Time for one agent loop iteration including status write.",
		Buckets:   prometheus.ExponentialBuckets(0.005, 2, 10),
	}, []string{"node"})

	// ClusterReclaimable is the controller-side rollup.
	ClusterReclaimable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "cluster",
		Name:      "reclaimable",
		Help:      "Cluster-wide reclaimable capacity (cpu millicores, memory bytes).",
	}, []string{"resource"})

	// ClusterOvercommitPercent is reclaimable capacity as a share of real
	// allocatable capacity: the single number that answers "how much extra
	// cluster did kqos manufacture?".
	ClusterOvercommitPercent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "cluster",
		Name:      "overcommit_percent",
		Help:      "Reclaimable capacity as a percentage of allocatable capacity.",
	})

	// NodeExtendedResourceSyncs counts patches to Node extended resources.
	NodeExtendedResourceSyncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "controller",
		Name:      "node_resource_syncs_total",
		Help:      "Patches applied to Node extended resources.",
	}, []string{"result"})

	// WebhookMutations counts admission decisions by outcome.
	WebhookMutations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "mutations_total",
		Help:      "Admission decisions, labelled by QoS level and action.",
	}, []string{"qos_level", "action"})

	// WorkloadWastePercent is the per-workload waste figure from profiling.
	WorkloadWastePercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "profile",
		Name:      "waste_percent",
		Help:      "Share of a workload's CPU request that it never uses.",
	}, []string{"namespace", "workload"})

	// UsageReportsTotal counts agent-to-controller usage pushes.
	UsageReportsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "usage",
		Name:      "reports_total",
		Help:      "Usage reports exchanged between agents and the controller.",
	}, []string{"direction", "result"})
)

// All is every collector kqos defines.
func All() []prometheus.Collector {
	return []prometheus.Collector{
		NodeReclaimable, NodeProtectedUsage,
		QoSUsage, QoSRequested, QoSPods,
		NodePressureLevel, NodePressureStall,
		EvictionsTotal, EvictionsProposedTotal, EvictionsSuppressedTotal, EvictionErrorsTotal,
		CollectDuration, CollectErrorsTotal, ReconcileDuration,
		ClusterReclaimable, ClusterOvercommitPercent, NodeExtendedResourceSyncs,
		WebhookMutations, WorkloadWastePercent, UsageReportsTotal,
	}
}

// MustRegister adds every kqos collector to the controller-runtime registry,
// which is already wired to the /metrics endpoint each binary serves.
func MustRegister() {
	metrics.Registry.MustRegister(All()...)
}

// PressureValue maps a pressure level string onto its numeric gauge value.
func PressureValue(level string) float64 {
	switch level {
	case "Critical":
		return 2
	case "Moderate":
		return 1
	default:
		return 0
	}
}
