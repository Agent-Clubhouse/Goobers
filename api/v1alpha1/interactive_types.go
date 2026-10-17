package v1alpha1

// GaggleInteractive is the additive DSL 3.0 interactive surface contract.
// It creates neither connections nor credentials. Existing workflow version pins
// continue to govern ordinary workflows independently of Gaggle.DSLVersion.
type GaggleInteractive struct {
	// Surfaces are optional entry points; an empty block exposes no chat.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Surfaces []InteractiveSurface `json:"surfaces,omitempty" yaml:"surfaces,omitempty"`
	// Authorities bind local names to opaque, non-secret host broker references.
	// They do not grant access; the broker resolves and authorizes each operation.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	Authorities []InteractiveAuthority `json:"authorities,omitempty" yaml:"authorities,omitempty"`
}

// InteractiveSurface binds a portal entry point to an existing Goober and a
// bounded set of mediated tools. Tools do not configure harness builtin tools.
// +kubebuilder:validation:XValidation:rule="self.defaultExecutionMode in self.executionModes",message="defaultExecutionMode must belong to executionModes"
type InteractiveSurface struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name" yaml:"name"`
	// +optional
	// +kubebuilder:validation:MaxLength=120
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=helper;workbench
	Mode string `json:"mode" yaml:"mode"`
	// Enabled defaults to true when omitted. A disabled surface admits no work.
	// +optional
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// GooberRef resolves in this gaggle or the existing instance-shared scope.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	GooberRef string `json:"gooberRef" yaml:"gooberRef"`
	// Tools is a default-deny allowlist for the host's mediated MCP bridge only.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['session_context', 'inspect_action', 'validate_workflow', 'propose_action', 'propose_child_workflow', 'read_work_item', 'query_telemetry'])",message="unknown interactive mediated tool"
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	// Workflows resolves within this gaggle, never by global name alone.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	Workflows []string `json:"workflows,omitempty" yaml:"workflows,omitempty"`
	// AuthorityRefs resolves against this GaggleInteractive.Authorities only.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	AuthorityRefs []string `json:"authorityRefs,omitempty" yaml:"authorityRefs,omitempty"`
	// ExecutionModes cannot expand the host's independently permitted modes.
	// local-tools is an explicitly selected fixture adapter, never a fallback.
	// +kubebuilder:validation:Required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['codex-local', 'local-tools'])",message="unknown interactive execution mode"
	ExecutionModes []string `json:"executionModes" yaml:"executionModes"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=codex-local;local-tools
	DefaultExecutionMode string `json:"defaultExecutionMode" yaml:"defaultExecutionMode"`
	// +kubebuilder:validation:Required
	Session InteractiveSessionPolicy `json:"session" yaml:"session"`
	// +optional
	ChildWorkflows *InteractiveChildWorkflowPolicy `json:"childWorkflows,omitempty" yaml:"childWorkflows,omitempty"`
}

// EffectiveEnabled reports the explicit or default surface admission setting.
func (s InteractiveSurface) EffectiveEnabled() bool { return s.Enabled == nil || *s.Enabled }

// InteractiveSessionPolicy declares ceilings; the host may narrow them further.
// +kubebuilder:validation:XValidation:rule="self.maxActivePerOwner <= self.maxActive",message="maxActivePerOwner must not exceed maxActive"
// +kubebuilder:validation:XValidation:rule="self.leaseSeconds <= self.maxTurnSeconds",message="leaseSeconds must not exceed maxTurnSeconds"
type InteractiveSessionPolicy struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	LeaseSeconds int32 `json:"leaseSeconds" yaml:"leaseSeconds"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	MaxTurnSeconds int32 `json:"maxTurnSeconds" yaml:"maxTurnSeconds"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	MaxChildren int32 `json:"maxChildren" yaml:"maxChildren"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxActive int32 `json:"maxActive" yaml:"maxActive"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxActivePerOwner int32 `json:"maxActivePerOwner" yaml:"maxActivePerOwner"`
}

// InteractiveChildWorkflowPolicy bounds reviewed children of interactive
// executions; it is distinct from the Workflow DSL 3.1 task.childWorkflows API.
type InteractiveChildWorkflowPolicy struct {
	// +kubebuilder:validation:Required
	Enabled bool `json:"enabled" yaml:"enabled"`
	// CommandCapabilities permits only these host-validated command families.
	// Empty permits no command workflow. It does not add arbitrary shell access.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['node-test', 'node-check'])",message="unknown child command capability"
	CommandCapabilities []string `json:"commandCapabilities,omitempty" yaml:"commandCapabilities,omitempty"`
}

// InteractiveAuthority is a non-secret lookup key, never a credential or grant.
type InteractiveAuthority struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name" yaml:"name"`
	// BrokerRef is resolved only by the host broker; URLs and inline secrets are
	// not a connection syntax. The local name carries no delegated authority.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._/-]*$`
	BrokerRef string `json:"brokerRef" yaml:"brokerRef"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=ado;app-insights
	Provider string `json:"provider" yaml:"provider"`
}
