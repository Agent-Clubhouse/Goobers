package sessioning

import "time"

// PRRepairChange is a bounded text change at the selected head. Nil Content
// means delete; absent PreviousBlob means add. Host/native checks remain required.
type PRRepairChange struct {
	Path         string  `json:"path"`
	PreviousBlob string  `json:"previousBlob,omitempty"`
	Content      *string `json:"content,omitempty"`
}

// PRRepairRequest never supplies the repository, actor or human selection. A
// parent command may advance only to this turn's exactly proven descendant.
type PRRepairRequest struct {
	RequestID       string           `json:"requestId"`
	ExpectedHeadSHA string           `json:"expectedHeadSha"`
	ParentCommandID string           `json:"parentCommandId,omitempty"`
	Rationale       string           `json:"rationale"`
	Changes         []PRRepairChange `json:"changes"`
}

// PRRepairOrigin is host-verified actual turn/journal custody, not model input.
type PRRepairOrigin struct {
	RunID            string `json:"runId"`
	SessionID        string `json:"sessionId"`
	TurnID           string `json:"turnId"`
	MessageID        string `json:"messageId"`
	MessageDigest    string `json:"messageDigest"`
	ConfigGeneration string `json:"configGeneration"`
	GooberDigest     string `json:"gooberDigest"`
	EnvelopeDigest   string `json:"envelopeDigest"`
	InputDigest      string `json:"inputDigest"`
}

// PRRepairReceipt separates native acknowledgement from exact observed commit
// evidence. Later authoritative observation is retained separately from this receipt.
type PRRepairReceipt struct {
	OperationDigest      string `json:"operationDigest"`
	Outcome              string `json:"outcome"`
	MutationAttempted    bool   `json:"mutationAttempted"`
	ProviderAcknowledged bool   `json:"providerAcknowledged"`
	ObservedMatches      bool   `json:"observedMatches"`
	CommitID             string `json:"commitId,omitempty"`
}

// PRRepairObservation is a read-only check by a current authorized human.
// Matches proves the exact original marker, ancestry and file delta, never acknowledgement.
type PRRepairObservation struct {
	Checker  Actor     `json:"checker"`
	At       time.Time `json:"at"`
	Matches  bool      `json:"matches"`
	CommitID string    `json:"commitId,omitempty"`
}

// PRRepairCommandView is safe retained evidence under current actor/target
// authorization. Commands are not workflow runs; RunID is the actual source turn.
type PRRepairCommandView struct {
	ID                  string                `json:"id"`
	SourceBindingID     string                `json:"sourceBindingId"`
	State               string                `json:"state"`
	RequestDigest       string                `json:"requestDigest"`
	OperationDigest     string                `json:"operationDigest"`
	SelectedHeadSHA     string                `json:"selectedHeadSha"`
	ExpectedHeadSHA     string                `json:"expectedHeadSha"`
	ParentCommandID     string                `json:"parentCommandId,omitempty"`
	RunID               string                `json:"runId"`
	Actor               Actor                 `json:"actor"`
	AcceptedAt          time.Time             `json:"acceptedAt"`
	AttemptedAt         *time.Time            `json:"attemptedAt,omitempty"`
	CompletedAt         *time.Time            `json:"completedAt,omitempty"`
	Receipt             *PRRepairReceipt      `json:"receipt,omitempty"`
	Observations        []PRRepairObservation `json:"observations,omitempty"`
	OmittedObservations int64                 `json:"omittedObservations,omitempty"`
}
