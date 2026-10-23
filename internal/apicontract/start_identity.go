package apicontract

// StartIdentity is proven from durable acceptance custody and a live lease.
type StartIdentity struct {
	AcceptanceID string
	RunID        string
	InstanceID   string
	StarterID    string
	OwnerEpoch   int64
}
