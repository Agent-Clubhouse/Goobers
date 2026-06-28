package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Environment is the deployment environment an instance represents. dev/prod are
// separate Goobers instances (separate infra+config) — see instance.md.
type Environment string

// Supported deployment environments. dev/staging/prod are separate instances.
const (
	EnvironmentDev     Environment = "dev"
	EnvironmentStaging Environment = "staging"
	EnvironmentProd    Environment = "prod"
)

// ManifestSpec is the Helm-like, top-level desired state for an instance: which
// environment, the named connections available to its gaggles, and which gaggles
// are active. It is the entrypoint a deploy/reconcile applies (CFG-002, CFG-004).
type ManifestSpec struct {
	// Instance identifies the deployment this manifest configures.
	// +kubebuilder:validation:Required
	Instance InstanceRef `json:"instance" yaml:"instance"`
	// Connections are the named, reusable links (with Key Vault-backed creds)
	// that gaggles/goobers reference. Declared once here.
	// +optional
	Connections []Connection `json:"connections,omitempty" yaml:"connections,omitempty"`
	// Gaggles lists the gaggle definitions (by metadata.name) included in this
	// instance's desired state. Each named gaggle MUST have a Gaggle object.
	// +optional
	Gaggles []string `json:"gaggles,omitempty" yaml:"gaggles,omitempty"`
}

// InstanceRef identifies a deployed Goobers instance/tenant.
type InstanceRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// +kubebuilder:validation:Enum=dev;staging;prod
	// +kubebuilder:validation:Required
	Environment Environment `json:"environment" yaml:"environment"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mf
// +kubebuilder:subresource:status

// Manifest is the top-level config-as-code object declaring an instance's desired
// state. Exactly one Manifest is expected per config repo.
type Manifest struct {
	metav1.TypeMeta   `json:",inline" yaml:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec ManifestSpec `json:"spec" yaml:"spec"`
}

// +kubebuilder:object:root=true

// ManifestList is a list of Manifest objects.
type ManifestList struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Items           []Manifest `json:"items" yaml:"items"`
}
