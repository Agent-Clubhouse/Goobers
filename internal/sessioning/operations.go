package sessioning

import "github.com/goobers/goobers/internal/workbench"

// Transport and custody bounds apply independently of model instructions.
const (
	// OperationPath is a turn-scoped machine plane, not the human session API.
	OperationPath            = "/api/v1/runs/{run}/session-operations"
	OperationTokenPrefix     = "goobers-session-operation."
	OperationIssuer          = "goobers/session-operation"
	MaxOperationRequestBytes = 16 << 10
	MaxOperationsPerTurn     = 64
	MaxOperationResultBytes  = workbench.MaxBacklogPageBytes
	MaxOperationTurnBytes    = 8 << 20
)

// BacklogReadRequest has no actor, gaggle, credentials, endpoint or provider
// target. The launcher fixes those independently of model-authored arguments.
type BacklogReadRequest struct {
	SourceBindingID string `json:"sourceBindingId"`
	workbench.BacklogItemRequest
}

// BacklogListRequest asks for one bounded provider window.
type BacklogListRequest struct {
	SourceBindingID string `json:"sourceBindingId"`
	workbench.BacklogPageRequest
}
