package v1alpha1

// ChildAgentProfile permits one exact selection for an existing allowed Goober.
// Selecting a profile never replaces that Goober's instructions or capabilities.
type ChildAgentProfile struct {
	// Name is the unique profile identifier within the parent policy.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]+$`
	Name string `json:"name" yaml:"name"`
	// Goober must also occur in the parent's allowedGoobers.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Goober string `json:"goober" yaml:"goober"`
	// Harness is a configured harness identifier, never an executable or flags.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]+$`
	Harness Harness `json:"harness" yaml:"harness"`
	// Model is a model identifier resolved by host admission for this harness.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^\S+$`
	Model string `json:"model" yaml:"model"`
}

// ChildBaseReference declares a same-repository branch or tag by name.
// Launch must resolve the ref to an immutable commit before child acceptance.
type ChildBaseReference struct {
	// Name is the unique selector within the parent policy.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]+$`
	Name string `json:"name" yaml:"name"`
	// Ref is a fully qualified branch or tag ref, not a revision expression.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^refs/(heads|tags)/.+$`
	Ref string `json:"ref" yaml:"ref"`
}
