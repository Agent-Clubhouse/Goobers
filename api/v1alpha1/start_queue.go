package v1alpha1

// StartQueuePolicy captures waiting deadlines from the accepted configuration.
// It never expires a potentially started effect or generated child invocation.
type StartQueuePolicy struct {
	// PendingDeadlineSeconds bounds unattempted ordinary, event, session and human
	// continuation starts. Omission uses seven days from durable acceptance.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7776000
	PendingDeadlineSeconds *int32 `json:"pendingDeadlineSeconds,omitempty" yaml:"pendingDeadlineSeconds,omitempty"`
	// ScheduledDeadlineSeconds bounds an unattempted scheduled worker occurrence.
	// Omission uses one hour; it does not change the scheduler's nominal fire time.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7776000
	ScheduledDeadlineSeconds *int32 `json:"scheduledDeadlineSeconds,omitempty" yaml:"scheduledDeadlineSeconds,omitempty"`
}
