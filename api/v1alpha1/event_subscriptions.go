package v1alpha1

// GaggleEvents defines local consumer subscriptions. Publication authorization
// and durable start admission are separate from these matching declarations.
type GaggleEvents struct {
	// Publishers explicitly allow named workflows to publish exact event types.
	// Omission disables workflow publication.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=workflow
	Publishers []EventPublisher `json:"publishers,omitempty" yaml:"publishers,omitempty"`

	// Subscriptions names workflows in this gaggle only.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Subscriptions []EventSubscription `json:"subscriptions,omitempty" yaml:"subscriptions,omitempty"`
}

// EventSubscription retains an exact workflow generation at event acceptance.
type EventSubscription struct {
	// Name identifies this consumer within its gaggle.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
	Name string `json:"name" yaml:"name"`
	// Workflow names an existing workflow in this same gaggle.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Workflow string `json:"workflow" yaml:"workflow"`
	// Filter requires every All condition and at least one Any condition when
	// Any is supplied. At least one list must be nonempty.
	Filter EventSubscriptionFilter `json:"filter" yaml:"filter"`
	// Debounce is disabled when omitted.
	// +optional
	Debounce *EventDebounce `json:"debounce,omitempty" yaml:"debounce,omitempty"`
}

// EventSubscriptionFilter is a bounded boolean expression over envelope
// attributes. Conditions can be negated; data predicates are not supported.
// +kubebuilder:validation:XValidation:rule="(has(self.all) && size(self.all) > 0) || (has(self.any) && size(self.any) > 0)",message="at least one all or any condition is required"
type EventSubscriptionFilter struct {
	// All contains conditions which must all match.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=15
	All []EventAttributeMatch `json:"all,omitempty" yaml:"all,omitempty"`
	// Any contains alternatives, at least one of which must match.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=15
	Any []EventAttributeMatch `json:"any,omitempty" yaml:"any,omitempty"`
}

// EventAttributeMatch compares one CloudEvents envelope attribute literally.
type EventAttributeMatch struct {
	// Attribute is an envelope field; extensions and data are not selectors.
	// +kubebuilder:validation:Enum=id;source;type;subject
	Attribute string `json:"attribute" yaml:"attribute"`
	// Equals is a literal case-sensitive match, not a regular expression.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Equals string `json:"equals" yaml:"equals"`
	// Not negates equality, including when an optional attribute is absent.
	// +optional
	Not bool `json:"not,omitempty" yaml:"not,omitempty"`
}

// EventDebounce chooses exactly one grouping key source. Missing selected
// attributes fail the delivery rather than joining an implicit empty group.
// +kubebuilder:validation:XValidation:rule="has(self.keyAttribute) != has(self.constantKey)",message="exactly one keyAttribute or constantKey is required"
type EventDebounce struct {
	// KeyAttribute chooses an envelope field as the group key.
	// +optional
	// +kubebuilder:validation:Enum=id;source;type;subject
	KeyAttribute string `json:"keyAttribute,omitempty" yaml:"keyAttribute,omitempty"`
	// ConstantKey explicitly groups every match under one configured key.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ConstantKey string `json:"constantKey,omitempty" yaml:"constantKey,omitempty"`
	// Window is the trailing quiet period, from 100ms through 5m; default 5s.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	Window string `json:"window,omitempty" yaml:"window,omitempty"`
	// MaxWait is the absolute group lifetime, at least Window and at most 1h;
	// default 30s. A longer Window requires an explicit compatible MaxWait.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	MaxWait string `json:"maxWait,omitempty" yaml:"maxWait,omitempty"`
	// MaxEvents closes the group at this count; default 100.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	MaxEvents *int32 `json:"maxEvents,omitempty" yaml:"maxEvents,omitempty"`
	// InputMode selects all payloads or the latest, preserving all memberships.
	// Defaults to all.
	// +optional
	// +kubebuilder:validation:Enum=all;latest
	InputMode string `json:"inputMode,omitempty" yaml:"inputMode,omitempty"`
}

// EventPublisher is an explicit workflow and event-type publication ceiling.
type EventPublisher struct {
	// Workflow names a workflow in this gaggle only.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Workflow string `json:"workflow" yaml:"workflow"`
	// AllowedTypes are literal CloudEvents types; wildcards are not interpreted.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	AllowedTypes []string `json:"allowedTypes" yaml:"allowedTypes"`
}
