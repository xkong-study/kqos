package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TargetReference points at the workload being profiled.
type TargetReference struct {
	// APIVersion of the target, e.g. "apps/v1".
	APIVersion string `json:"apiVersion"`

	// Kind of the target, e.g. "Deployment".
	Kind string `json:"kind"`

	// Name of the target in the profile's namespace.
	Name string `json:"name"`
}

// WorkloadProfileSpec declares what to profile and how.
type WorkloadProfileSpec struct {
	// TargetRef is the workload whose pods are sampled.
	TargetRef TargetReference `json:"targetRef"`

	// QoSLevel the workload's pods run at. Used to decide whether the
	// recommendation should be conservative (guaranteed tiers) or aggressive
	// (reclaimed tier).
	// +kubebuilder:default=shared_cores
	QoSLevel QoSLevel `json:"qosLevel,omitempty"`

	// SafetyMarginPercent is added on top of the observed percentile before it
	// becomes a recommendation.
	// +kubebuilder:default=20
	SafetyMarginPercent int32 `json:"safetyMarginPercent,omitempty"`
}

// UsageStatistics is the distribution of one resource over the sample window.
type UsageStatistics struct {
	P50 resource.Quantity `json:"p50,omitempty"`
	P90 resource.Quantity `json:"p90,omitempty"`
	P95 resource.Quantity `json:"p95,omitempty"`
	P99 resource.Quantity `json:"p99,omitempty"`
	Max resource.Quantity `json:"max,omitempty"`
}

// WorkloadProfileStatus is the profiling result and the resource
// recommendation derived from it.
type WorkloadProfileStatus struct {
	// Samples is how many observations back the statistics.
	// +optional
	Samples int64 `json:"samples,omitempty"`

	// PodCount is how many pods of the workload were seen in the last pass.
	// +optional
	PodCount int32 `json:"podCount,omitempty"`

	// CPU is the per-pod CPU usage distribution.
	CPU UsageStatistics `json:"cpu,omitempty"`

	// Memory is the per-pod memory usage distribution.
	Memory UsageStatistics `json:"memory,omitempty"`

	// CurrentRequests is the sum of requests currently declared per pod.
	CurrentRequests corev1.ResourceList `json:"currentRequests,omitempty"`

	// Recommendation is what kqos believes the pod should request. Advisory
	// only: kqos reports it, a human or an autoscaler applies it.
	Recommendation corev1.ResourceList `json:"recommendation,omitempty"`

	// WastePercent is how much of the current request the workload never
	// touches. This is the number that justifies the whole reclaimed tier.
	// +optional
	WastePercent int32 `json:"wastePercent,omitempty"`

	// Confidence is Low until the window is full, then Medium, then High once
	// the workload has been observed across a full traffic cycle.
	// +kubebuilder:validation:Enum=Low;Medium;High
	Confidence string `json:"confidence,omitempty"`

	// LastUpdated is when the statistics were last recomputed.
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wp,categories=kqos
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="QoS",type=string,JSONPath=`.spec.qosLevel`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.podCount`
// +kubebuilder:printcolumn:name="CPU-P95",type=string,JSONPath=`.status.cpu.p95`
// +kubebuilder:printcolumn:name="Rec-CPU",type=string,JSONPath=`.status.recommendation.cpu`
// +kubebuilder:printcolumn:name="Waste%",type=integer,JSONPath=`.status.wastePercent`
// +kubebuilder:printcolumn:name="Confidence",type=string,JSONPath=`.status.confidence`

// WorkloadProfile is the service-profiling record for one workload: what it
// asks for, what it actually uses, and what it should ask for instead.
type WorkloadProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadProfileSpec   `json:"spec,omitempty"`
	Status WorkloadProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadProfileList contains a list of WorkloadProfile.
type WorkloadProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadProfile{}, &WorkloadProfileList{})
}
