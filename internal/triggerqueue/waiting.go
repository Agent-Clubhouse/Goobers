package triggerqueue

import "context"

// WaitingReason is a closed safe vocabulary, never a provider error or payload.
type WaitingReason string

// Waiting causes describe retained accepted custody without implying a start.
const (
	WaitingCapacity    WaitingReason = "capacity"
	WaitingAccess      WaitingReason = "current_access"
	WaitingSource      WaitingReason = "source_changed"
	WaitingSourceBusy  WaitingReason = "source_busy"
	WaitingUnsupported WaitingReason = "unsupported"
	WaitingValidation  WaitingReason = "validation_unavailable"
)

// Message is safe for shared queue views and command receipts.
func (r WaitingReason) Message() string {
	switch r {
	case WaitingCapacity:
		return "Waiting for workflow capacity or budget."
	case WaitingAccess:
		return "Waiting for current gaggle permission and configured credentials."
	case WaitingSource:
		return "The accepted source changed; this request remains queued for review."
	case WaitingSourceBusy:
		return "Waiting for the prior execution or source owner to release custody."
	case WaitingUnsupported:
		return "Waiting for supported execution wiring on this daemon."
	case WaitingValidation:
		return "Waiting for source validation; retry checks retain the same request."
	default:
		return ""
	}
}

// SetWaitingReason changes observation only while the request is unattempted.
// Losing a race with a dispatch/terminal transition is harmless; no custody
// transition, clock reset, or authority change is permitted by this operation.
func (s *Store) SetWaitingReason(ctx context.Context, id string, reason WaitingReason) error {
	message := reason.Message()
	if message == "" || id == "" || len(id) > 128 {
		return ErrTransition
	}
	_, err := s.db.ExecContext(ctx, `UPDATE triggers SET reason=? WHERE id=? AND state='accepted' AND finished_ns IS NULL`, message, id)
	return err
}
