package workbench

import "time"

// CommandActor identifies the verified human author; it is never a provider login.
type CommandActor struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// BacklogEditCommand is bounded human command evidence, not workflow execution.
// Unknown/attempting never authorize retry. Provider acknowledgement and a
// matching observation remain separate in Receipt, including after restart.
type BacklogEditCommand struct {
	ID              string               `json:"id"`
	Gaggle          string               `json:"gaggle"`
	SourceBindingID string               `json:"sourceBindingId"`
	Actor           CommandActor         `json:"actor"`
	ItemID          string               `json:"itemId"`
	SourceID        string               `json:"sourceId"`
	Field           string               `json:"field"`
	State           string               `json:"state"`
	Duplicate       bool                 `json:"duplicate"`
	RequestDigest   string               `json:"requestDigest"`
	OperationDigest string               `json:"operationDigest"`
	AcceptedAt      time.Time            `json:"acceptedAt"`
	AttemptedAt     *time.Time           `json:"attemptedAt,omitempty"`
	CompletedAt     *time.Time           `json:"completedAt,omitempty"`
	Receipt         *BacklogPatchReceipt `json:"receipt,omitempty"`
	NextAction      string               `json:"nextAction"`
}
