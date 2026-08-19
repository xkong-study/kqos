package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OvercommitConfig controls how aggressively unused capacity is resold.
type OvercommitConfig struct {
	// CPUTargetUtilizationPercent is the node CPU utilisation kqos aims for
	// after reclaimed pods are packed on. Reclaimable CPU is computed as
	// (target% of allocatable) - (actual usage of the guaranteed tiers).
	// +kubebuilder:default=75
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=95
	CPUTargetUtilizationPercent int32 `json:"cpuTargetUtilizationPercent"`

	// MemoryTargetUtilizationPercent is the same target for memory. Memory is
	// incompressible, so this should stay well below the CPU target.
	// +kubebuilder:default=65
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=90
	MemoryTargetUtilizationPercent int32 `json:"memoryTargetUtilizationPercent"`

	// HeadroomPercent is withheld from the reclaimable figure to absorb a burst
	// from the guaranteed tiers between two advisor ticks. This is the single
	// most important safety knob in kqos.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	HeadroomPercent int32 `json:"headroomPercent"`

	// MaxReclaimRatioPercent caps reclaimable resources at this fraction of
	// allocatable, no matter what the usage maths says. Protects against a node
	// that is genuinely idle being packed to the point where a single traffic
	// spike cannot be absorbed.
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxReclaimRatioPercent int32 `json:"maxReclaimRatioPercent"`

	// MinReclaimCPU is a floor below which reclaimable CPU is reported as zero,
	// avoiding churn from advertising slivers of a core.
	MinReclaimCPU *resource.Quantity `json:"minReclaimCpu,omitempty"`

	// MinReclaimMemory is the equivalent floor for memory.
	MinReclaimMemory *resource.Quantity `json:"minReclaimMemory,omitempty"`
}

// AdvisorConfig tunes the smoothing applied to raw cgroup samples.
type AdvisorConfig struct {
	// WindowSeconds is how much history the advisor keeps per node.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=30
	WindowSeconds int32 `json:"windowSeconds"`

	// IntervalSeconds is the sampling period.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds"`

	// Algorithm selects how the window is collapsed into one number.
	// +kubebuilder:default=p95
	// +kubebuilder:validation:Enum=p95;p99;max;ewma
	Algorithm string `json:"algorithm"`

	// EWMAAlpha is the smoothing factor when Algorithm is "ewma".
	// +kubebuilder:default="0.3"
	EWMAAlpha string `json:"ewmaAlpha,omitempty"`
}

// EvictionConfig configures the node-local eviction manager.
type EvictionConfig struct {
	// Enabled turns the eviction manager on. When false the manager still runs
	// and emits metrics but never evicts, which is how you validate thresholds
	// against production traffic before arming them.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// DryRun evaluates every plugin and records the verdict without calling the
	// eviction API.
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun"`

	// MemoryPressureThresholdPercent is the node memory utilisation above which
	// the memory-pressure plugin starts selecting victims.
	// +kubebuilder:default=85
	MemoryPressureThresholdPercent int32 `json:"memoryPressureThresholdPercent"`

	// CPUPressureThresholdPercent is the equivalent CPU trigger. CPU is
	// compressible so this is deliberately high: throttling is preferred to
	// eviction, and eviction only happens when throttling has stopped working.
	// +kubebuilder:default=92
	CPUPressureThresholdPercent int32 `json:"cpuPressureThresholdPercent"`

	// CPUSomeStalledThresholdPercent triggers on PSI rather than utilisation,
	// catching contention on nodes that never look busy.
	// +kubebuilder:default=40
	CPUSomeStalledThresholdPercent int32 `json:"cpuSomeStalledThresholdPercent"`

	// MaxEvictionsPerMinute rate-limits the manager so a metrics glitch cannot
	// drain a node.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	MaxEvictionsPerMinute int32 `json:"maxEvictionsPerMinute"`

	// GracePeriodSeconds is passed to the eviction API.
	// +kubebuilder:default=30
	GracePeriodSeconds int64 `json:"gracePeriodSeconds"`

	// StabilisationSeconds is how long a threshold must stay breached before the
	// first eviction, preventing reaction to a single bad sample.
	// +kubebuilder:default=30
	StabilisationSeconds int32 `json:"stabilisationSeconds"`

	// DisabledPlugins names eviction plugins to skip.
	DisabledPlugins []string `json:"disabledPlugins,omitempty"`
}

// CPUSetConfig controls how the agent partitions the node's CPUs.
type CPUSetConfig struct {
	// Enabled turns on cpuset actuation. Off by default because it writes to
	// the host cgroup tree.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// ReservedSystemCPUs is a cpuset expression kept for system_cores and the
	// kubelet itself, e.g. "0-1".
	ReservedSystemCPUs string `json:"reservedSystemCpus,omitempty"`

	// SharedPoolMinCPUs guarantees the shared tier never shrinks below this many
	// CPUs regardless of dedicated demand.
	// +kubebuilder:default=2
	SharedPoolMinCPUs int32 `json:"sharedPoolMinCpus"`

	// ReclaimedPoolMinCPUs is the same floor for the reclaimed tier.
	// +kubebuilder:default=1
	ReclaimedPoolMinCPUs int32 `json:"reclaimedPoolMinCpus"`
}

// WebhookConfig controls admission behaviour.
type WebhookConfig struct {
	// DefaultQoSLevel is applied to pods that carry no QoS annotation.
	// +kubebuilder:default=shared_cores
	DefaultQoSLevel QoSLevel `json:"defaultQoSLevel"`

	// RewriteReclaimedResources makes the webhook translate cpu/memory requests
	// on reclaimed pods into kqos.io/reclaimed-* extended resources, so the
	// native scheduler never counts them against real capacity.
	// +kubebuilder:default=true
	RewriteReclaimedResources bool `json:"rewriteReclaimedResources"`

	// ExemptNamespaces are never mutated.
	ExemptNamespaces []string `json:"exemptNamespaces,omitempty"`
}

// QoSPolicySpec is the whole tunable surface of kqos, in one object. Changing
// it takes effect without restarting any component: the agent and controller
// both watch it and swap configuration in place.
type QoSPolicySpec struct {
	// NodeSelector limits this policy to a subset of nodes. Empty means all.
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`

	Overcommit OvercommitConfig `json:"overcommit"`
	Advisor    AdvisorConfig    `json:"advisor"`
	Eviction   EvictionConfig   `json:"eviction"`
	CPUSet     CPUSetConfig     `json:"cpuSet"`
	Webhook    WebhookConfig    `json:"webhook"`
}

// QoSPolicyStatus is the cluster-wide rollup maintained by the controller.
type QoSPolicyStatus struct {
	// ObservedNodes is how many NodeResourceProfiles matched the selector.
	// +optional
	ObservedNodes int32 `json:"observedNodes,omitempty"`

	// ReadyNodes is how many of those reported a healthy agent recently.
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// TotalReclaimableCPU is the sum of reclaimable CPU across matched nodes.
	TotalReclaimableCPU resource.Quantity `json:"totalReclaimableCpu,omitempty"`

	// TotalReclaimableMemory is the memory equivalent.
	TotalReclaimableMemory resource.Quantity `json:"totalReclaimableMemory,omitempty"`

	// EffectiveOvercommitPercent is reclaimable capacity as a percentage of
	// real allocatable capacity: the headline number for how much extra cluster
	// kqos has manufactured.
	// +optional
	EffectiveOvercommitPercent int32 `json:"effectiveOvercommitPercent,omitempty"`

	// NodesUnderPressure counts nodes not at PressureNone.
	// +optional
	NodesUnderPressure int32 `json:"nodesUnderPressure,omitempty"`

	// LastUpdated is when this rollup was computed.
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=qp,categories=kqos
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.observedNodes`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Reclaim-CPU",type=string,JSONPath=`.status.totalReclaimableCpu`
// +kubebuilder:printcolumn:name="Reclaim-Mem",type=string,JSONPath=`.status.totalReclaimableMemory`
// +kubebuilder:printcolumn:name="Overcommit%",type=integer,JSONPath=`.status.effectiveOvercommitPercent`
// +kubebuilder:printcolumn:name="Pressure",type=integer,JSONPath=`.status.nodesUnderPressure`

// QoSPolicy is the cluster-wide, hot-reloadable configuration for kqos.
type QoSPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   QoSPolicySpec   `json:"spec,omitempty"`
	Status QoSPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// QoSPolicyList contains a list of QoSPolicy.
type QoSPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []QoSPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&QoSPolicy{}, &QoSPolicyList{})
}
