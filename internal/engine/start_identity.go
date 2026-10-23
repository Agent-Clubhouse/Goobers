package engine

// StartIdentity is read from durable acceptance custody and the live lease.
// A matching pair of caller-supplied strings is never a StartIdentity proof.
type StartIdentity struct {
	AcceptanceID string
	RunID        string
	InstanceID   string
	StarterID    string
	OwnerEpoch   int64
}
