package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PressureLevel summarises how close a node is to resource exhaustion.
type PressureLevel string

const (
	PressureNone     PressureLevel = "None"
	PressureModerate PressureLevel = "Moderate"
	PressureCritical PressureLevel = "Critical"
)

// NodeResourceProfileSpec is intentionally thin: the profile is a report, and
// the only thing the control plane writes into it is which node it describes.
type NodeResourceProfileSpec struct {
	// NodeName is the node this profile describes. Always equal to the object
	// name; carried in the spec so consumers do not have to rely on naming.
	NodeName string `json:"nodeName"`
}

// QoSAllocation is the per-class breakdown of what was promised versus what is
// actually being consumed. The gap between the two is the source of every
// reclaimable core kqos hands out.
type QoSAllocation struct {
	// Pods is the number of running pods in this class.
	// +optional
	Pods int32 `json:"pods,omitempty"`

	// Requested is the sum of container requests across the class.
	Requested corev1.ResourceList `json:"requested,omitempty"`

	// Limits is the sum of container limits across the class.
	Limits corev1.ResourceList `json:"limits,omitempty"`

	// Actual is measured consumption read from cgroup v2, smoothed by the
	// advisor's window.
	Actual corev1.ResourceList `json:"actual,omitempty"`
}

// CPUSetPool is one contiguous CPU assignment managed by the agent.
type CPUSetPool struct {
	// Name identifies the pool: "reserved", "shared", "reclaimed", or the pod
	// UID for a dedicated assignment.
	Name string `json:"name"`

	// QoSLevel is the class this pool serves.
	QoSLevel QoSLevel `json:"qosLevel"`

	// CPUs is the Linux cpuset expression currently applied, e.g. "0-3,8".
	CPUs string `json:"cpus"`

	// Size is the number of CPUs the expression resolves to.
	Size int32 `json:"size"`
}

// NodePressure is the agent's read on how stressed the node is right now.
type NodePressure struct {
	// Level is the coarse classification the eviction manager acts on.
	// +optional
	Level PressureLevel `json:"level,omitempty"`

	// CPUUtilizationPercent is node-wide CPU consumption, 0-100.
	// +optional
	CPUUtilizationPercent int32 `json:"cpuUtilizationPercent,omitempty"`

	// MemoryUtilizationPercent is node-wide memory consumption, 0-100.
	// +optional
	MemoryUtilizationPercent int32 `json:"memoryUtilizationPercent,omitempty"`

	// CPUSomeStalledPercent is the PSI "cpu some avg10" value, which detects
	// contention that raw utilisation misses: a node can be at 70% CPU and still
	// be starving latency-critical threads.
	CPUSomeStalledPercent int32 `json:"cpuSomeStalledPercent,omitempty"`

	// MemoryFullStalledPercent is the PSI "memory full avg10" value.
	MemoryFullStalledPercent int32 `json:"memoryFullStalledPercent,omitempty"`
}

// NodeResourceProfileStatus is written exclusively by the node agent.
type NodeResourceProfileStatus struct {
	// Capacity mirrors the node's physical resources.
	Capacity corev1.ResourceList `json:"capacity,omitempty"`

	// Allocatable mirrors the node's allocatable resources.
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`

	// QoSAllocations breaks allocation down by class.
	QoSAllocations map[QoSLevel]QoSAllocation `json:"qosAllocations,omitempty"`

	// Reclaimable is the headroom the advisor is willing to oversell. The
	// overcommit controller copies this onto the Node as extended resources.
	Reclaimable corev1.ResourceList `json:"reclaimable,omitempty"`

	// Pressure is the current stress classification.
	Pressure NodePressure `json:"pressure,omitempty"`

	// CPUSetPools records the CPU partitioning the agent has applied.
	CPUSetPools []CPUSetPool `json:"cpuSetPools,omitempty"`

	// TopologyZones describes the NUMA layout used for dedicated placement.
	TopologyZones []TopologyZone `json:"topologyZones,omitempty"`

	// AdvisorRevision increments on every recommendation change, giving
	// consumers a cheap way to detect staleness.
	AdvisorRevision int64 `json:"advisorRevision,omitempty"`

	// LastReportTime is when the agent last refreshed this status.
	LastReportTime metav1.Time `json:"lastReportTime,omitempty"`

	// Conditions carries AgentHealthy and TopologyReady.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TopologyZone is one NUMA node as seen by the agent.
type TopologyZone struct {
	// ID is the NUMA node id.
	ID int32 `json:"id"`

	// CPUs is the cpuset expression of CPUs in this zone.
	CPUs string `json:"cpus"`

	// AllocatableCPUs is how many CPUs in the zone are not already dedicated.
	AllocatableCPUs int32 `json:"allocatableCpus"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nrp,categories=kqos
// +kubebuilder:printcolumn:name="Pressure",type=string,JSONPath=`.status.pressure.level`
// +kubebuilder:printcolumn:name="CPU%",type=integer,JSONPath=`.status.pressure.cpuUtilizationPercent`
// +kubebuilder:printcolumn:name="Mem%",type=integer,JSONPath=`.status.pressure.memoryUtilizationPercent`
// +kubebuilder:printcolumn:name="Reclaim-CPU",type=string,JSONPath=`.status.reclaimable.cpu`
// +kubebuilder:printcolumn:name="Reclaim-Mem",type=string,JSONPath=`.status.reclaimable.memory`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NodeResourceProfile is the per-node view of what was promised, what is
// actually used, and how much of the difference kqos is prepared to resell.
// One object per node, written by the agent DaemonSet, read by the overcommit
// controller and the scheduler plugin.
type NodeResourceProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeResourceProfileSpec   `json:"spec,omitempty"`
	Status NodeResourceProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeResourceProfileList contains a list of NodeResourceProfile.
type NodeResourceProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeResourceProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeResourceProfile{}, &NodeResourceProfileList{})
}
