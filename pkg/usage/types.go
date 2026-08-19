// Package usage carries per-pod consumption from the node agents to the
// controller.
//
// This is kqos's data plane, and it is deliberately not the Kubernetes API.
// Per-pod usage is high-frequency, high-cardinality and worthless once it is a
// minute old; writing it through etcd would put a write amplification of
// (pods x nodes x sample rate) onto the cluster's most contended component in
// order to store data that is discarded almost immediately. So the control
// plane -- policy, recommendations, node profiles -- goes through CRDs, and
// the measurement stream goes over a direct HTTP path that can drop samples
// without anybody caring.
package usage

import "time"

// PodUsage is one pod's footprint at one instant.
type PodUsage struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	QoSLevel  string `json:"qosLevel"`

	// OwnerKind and OwnerName are the pod's immediate controller, e.g.
	// ("ReplicaSet", "web-7d9f"). The controller resolves that to the workload
	// a human recognises; the agent deliberately does not, because doing so
	// would require every agent to watch every ReplicaSet in the cluster.
	OwnerKind string `json:"ownerKind,omitempty"`
	OwnerName string `json:"ownerName,omitempty"`

	CPUMilli    float64 `json:"cpuMilli"`
	MemoryBytes uint64  `json:"memoryBytes"`

	RequestCPUMilli    int64 `json:"requestCpuMilli"`
	RequestMemoryBytes int64 `json:"requestMemoryBytes"`

	// ThrottledRatio is the fraction of CFS periods the pod spent throttled.
	ThrottledRatio float64 `json:"throttledRatio,omitempty"`
}

// Report is one agent's view of its node at one instant.
type Report struct {
	Node      string     `json:"node"`
	Timestamp time.Time  `json:"timestamp"`
	Pods      []PodUsage `json:"pods"`

	// Degraded marks a report built from estimates rather than measurements.
	Degraded bool `json:"degraded,omitempty"`
}

// WorkloadKey identifies the workload a set of pods belongs to.
type WorkloadKey struct {
	Namespace string
	Kind      string
	Name      string
}

// String renders the key for logs and map indexing.
func (k WorkloadKey) String() string { return k.Namespace + "/" + k.Kind + "/" + k.Name }

// Stats is the distribution of one resource across a workload's pods over the
// retention window.
type Stats struct {
	Samples int64
	P50     float64
	P90     float64
	P95     float64
	P99     float64
	Max     float64
	Mean    float64
}
