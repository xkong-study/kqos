package v1alpha1

// QoSLevel is the kqos service-quality class of a pod. It is a superset of the
// native Kubernetes QoS classes: where Kubernetes derives QoS from the
// request/limit relationship, kqos treats it as a declared intent that drives
// CPU topology placement, overcommit accounting and eviction order.
type QoSLevel string

const (
	// QoSDedicatedCores gives the pod exclusive CPUs that no other pod may run
	// on. Used by latency-critical serving workloads that cannot tolerate
	// noisy-neighbour interference.
	QoSDedicatedCores QoSLevel = "dedicated_cores"

	// QoSSharedCores places the pod in a shared CPU pool alongside other
	// shared_cores pods. The default for ordinary microservices.
	QoSSharedCores QoSLevel = "shared_cores"

	// QoSReclaimedCores runs the pod on capacity that kqos has determined is
	// unused by the higher classes. It is the oversold tier: cheap, throttled
	// first, and evicted first.
	QoSReclaimedCores QoSLevel = "reclaimed_cores"

	// QoSSystemCores is reserved for node-level system agents that must keep
	// running even under pressure. Never evicted by kqos.
	QoSSystemCores QoSLevel = "system_cores"
)

// Annotations and labels understood by kqos.
const (
	// AnnotationQoSLevel declares the pod's QoS level. Set by the user, defaulted
	// and validated by the kqos webhook.
	AnnotationQoSLevel = "kqos.io/qos-level"

	// AnnotationNUMABinding requests that a dedicated_cores pod be confined to a
	// single NUMA node. Value "true".
	AnnotationNUMABinding = "kqos.io/numa-binding"

	// AnnotationCPUSetPool names the shared pool a shared_cores pod belongs to,
	// allowing coarse isolation between tenants inside the shared tier.
	AnnotationCPUSetPool = "kqos.io/cpuset-pool"

	// AnnotationOriginalResources records the pod's pre-mutation resource block
	// so the reclaimed-tier rewrite performed by the webhook stays auditable.
	AnnotationOriginalResources = "kqos.io/original-resources"

	// LabelManaged marks objects created or rewritten by kqos.
	LabelManaged = "kqos.io/managed"
)

// Extended resources advertised on Node objects by the overcommit controller.
// Reclaimed pods request these instead of cpu/memory, which keeps the native
// scheduler's accounting of the guaranteed tiers completely untouched.
const (
	// ResourceReclaimedCPU is advertised in milli-cores. Extended resources must
	// be integers, so 1 unit == 1 milli-core.
	ResourceReclaimedCPU = "kqos.io/reclaimed-cpu"

	// ResourceReclaimedMemory is advertised in mebibytes.
	ResourceReclaimedMemory = "kqos.io/reclaimed-memory"
)

// DefaultQoSPolicyName is the name of the singleton cluster QoSPolicy.
const DefaultQoSPolicyName = "default"

// KnownQoSLevels lists every valid QoS level, ordered from most to least
// privileged. Eviction walks this list in reverse.
var KnownQoSLevels = []QoSLevel{
	QoSSystemCores,
	QoSDedicatedCores,
	QoSSharedCores,
	QoSReclaimedCores,
}

// IsValid reports whether the level is one kqos knows about.
func (q QoSLevel) IsValid() bool {
	for _, k := range KnownQoSLevels {
		if k == q {
			return true
		}
	}
	return false
}

// EvictionRank orders levels for eviction: the highest rank is evicted first.
func (q QoSLevel) EvictionRank() int {
	switch q {
	case QoSReclaimedCores:
		return 3
	case QoSSharedCores:
		return 2
	case QoSDedicatedCores:
		return 1
	case QoSSystemCores:
		return 0
	default:
		return 2
	}
}

// Evictable reports whether kqos may evict pods at this level at all.
func (q QoSLevel) Evictable() bool {
	return q != QoSSystemCores
}
