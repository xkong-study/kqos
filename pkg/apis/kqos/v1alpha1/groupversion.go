// Package v1alpha1 contains the kqos API types.
// +kubebuilder:object:generate=true
// +groupName=kqos.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "kqos.io", Version: "v1alpha1"}

// SchemeBuilder registers the kqos types into a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the kqos types to a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme

// Resource qualifies an unqualified resource name with the kqos group.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
