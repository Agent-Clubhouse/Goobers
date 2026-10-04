package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ErrWorkerOccupancy means a concurrent acceptance already owns the missing
// worker capacity. It is a normal retryable observation, not an execution failure.
var ErrWorkerOccupancy = errors.New("triggerqueue: queued worker capacity changed")

// WorkflowPendingLimit is trusted scheduler evidence used only inside acceptance.
// ActiveRunIDs are exact runs already included in the scheduler's live count.
type WorkflowPendingLimit struct {
	Gaggle, Workflow string
	MaxPending       int
	ActiveRunIDs     []string
}

// PendingWorkflowStarts counts every queued start charged to this workflow,
// including manual/event/child starts, without counting an active run twice.
func (s *Store) PendingWorkflowStarts(ctx context.Context, limit WorkflowPendingLimit) (int, error) {
	if err := validatePendingLimit(limit); err != nil {
		return 0, err
	}
	return pendingWorkflowStarts(ctx, s.db, limit)
}
func validatePendingLimit(limit WorkflowPendingLimit) error {
	if !validChildText(limit.Gaggle, 256, true) || !validChildText(limit.Workflow, 256, true) || limit.MaxPending < 0 || len(limit.ActiveRunIDs) > MaxRecords {
		return errors.New("triggerqueue: invalid worker occupancy scope")
	}
	for _, id := range limit.ActiveRunIDs {
		if !validChildText(id, 256, true) {
			return errors.New("triggerqueue: invalid active worker identity")
		}
	}
	return nil
}
func pendingWorkflowStarts(ctx context.Context, q sourceQuerier, limit WorkflowPendingLimit) (int, error) {
	active, err := json.Marshal(append([]string{}, limit.ActiveRunIDs...))
	if err != nil {
		return 0, err
	}
	var invalid int
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM triggers WHERE state IN ('accepted','dispatching') AND NOT json_valid(payload)`).Scan(&invalid); err != nil {
		return 0, err
	}
	if invalid > 0 {
		return 0, errors.New("triggerqueue: cannot prove occupancy of malformed accepted starts")
	}
	var count int
	// Legacy unqualified selections conservatively occupy the same name in each
	// gaggle until dispatch resolves their scope. Generated children consume the
	// parent's configured workflow bucket, not their model-authored display name.
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM triggers
 WHERE state IN ('accepted','dispatching')
 AND NOT EXISTS(SELECT 1 FROM direct_engine_inputs i WHERE i.acceptance_id=triggers.id)
 AND COALESCE(json_extract(payload,'$.target.gaggle'),json_extract(payload,'$.gaggle'),json_extract(payload,'$.request.gaggle'),'') IN (?, '')
 AND COALESCE(json_extract(payload,'$.target.workflow'),json_extract(payload,'$.parentWorkflow'),json_extract(payload,'$.workflow'),json_extract(payload,'$.request.workflow'),'')=?
 AND substr(id,9) NOT IN (SELECT value FROM json_each(?))`, limit.Gaggle, limit.Workflow, string(active)).Scan(&count)
	return count, err
}
func checkWorkerOccupancy(ctx context.Context, tx *sql.Tx, b SourceBatch) error {
	if b.PendingLimit == nil {
		return nil
	}
	if err := validatePendingLimit(*b.PendingLimit); err != nil {
		return err
	}
	count, err := pendingWorkflowStarts(ctx, tx, *b.PendingLimit)
	if err != nil {
		return err
	}
	if count+len(b.Starts) > b.PendingLimit.MaxPending {
		return ErrWorkerOccupancy
	}
	return nil
}
