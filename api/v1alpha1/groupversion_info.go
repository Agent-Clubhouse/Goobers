// Package v1alpha1 contains the canonical, typed definitions for the Goobers
// control plane: the CRD types for the primitives (Gaggle, Goober, Workflow,
// with Task and Gate as states within a Workflow) plus the wire envelopes
// (invocation, result, verdict) that every component exchanges at runtime.
//
// These types are the single source of truth referenced across the platform.
// The operator (controller-runtime) consumes the CRD types; the scheduler,
// providers, gates, and goober runtime consume the envelope types.
//
// DeepCopy methods and CRD YAML manifests are generated from these types by
// controller-gen in the operator build (Dev-3); this package intentionally only
// declares the types + group/version so it compiles standalone.
//
// +kubebuilder:object:generate=true
// +groupName=goobers.dev
package v1alpha1

import "k8s.io/apimachinery/pkg/runtime/schema"

// GroupName is the API group for all Goobers config-as-code definitions.
const GroupName = "goobers.dev"

// Version is the API version of this package.
const Version = "v1alpha1"

// GroupVersion is the group/version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// Resource takes an unqualified resource and returns a Group-qualified
// GroupResource.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

// RegisteredKinds lists the CRD kinds defined by this package, in dependency
// order (Manifest references Gaggles; Goobers and Workflows belong to Gaggles).
func RegisteredKinds() []string {
	return []string{"Manifest", "Gaggle", "Goober", "Workflow"}
}
