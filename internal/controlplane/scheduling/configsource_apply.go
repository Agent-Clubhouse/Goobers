package scheduling

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// WorkflowSourceApplier serializes explicit and background source application.
type WorkflowSourceApplier struct {
	Source   instance.WorkflowSource
	mu       sync.Mutex
	Prepare  func(context.Context, string) (string, bool, *instance.PreparedConfigSwap, error)
	Reloader SourceReloader
	revision string
}

// Apply applies the configured source and reports the resulting revision.
func (a *WorkflowSourceApplier) Apply(ctx context.Context, now time.Time) ApplyResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	result, revision, _, err := a.sync(ctx, "", now)
	response := ApplyResponse{
		Applied: result.Applied, OldDigest: result.OldDigest, NewDigest: result.NewDigest,
		Rejected: result.Rejected, Revision: revision,
	}
	if err != nil {
		response.Error = err.Error()
	} else if revision != "" {
		a.revision = revision
	}
	return response
}

// Reconcile applies the source when its revision has changed.
func (a *WorkflowSourceApplier) Reconcile(ctx context.Context, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, revision, changed, err := a.sync(ctx, a.revision, now)
	if err != nil {
		return err
	}
	if changed {
		a.revision = revision
	}
	return nil
}

func (a *WorkflowSourceApplier) sync(ctx context.Context, currentRevision string, now time.Time) (SourceReloadResult, string, bool, error) {
	revision, changed, swap, err := a.Prepare(ctx, currentRevision)
	if err != nil {
		return SourceReloadResult{}, "", false, fmt.Errorf("sync workflow source: %w", err)
	}
	if !changed {
		return SourceReloadResult{}, revision, false, nil
	}
	result, err := ApplyPreparedConfigSource(a.Reloader, swap, now)
	return result, revision, true, err
}

// SourceReloadResult describes validation and publication of a source candidate.
type SourceReloadResult struct {
	Applied              bool
	OldDigest, NewDigest string
	Rejected             string
}

// ApplyPreparedConfigSource closes the filesystem/apply transaction. A
// validation rejection restores the last applied tree while preserving the
// reloader's rejected-generation status and journal event. Other apply errors
// retain the candidate, matching the pre-existing explicit divergence state:
// they may have occurred after a non-transactional runtime side effect.
func ApplyPreparedConfigSource(reloader SourceReloader, swap *instance.PreparedConfigSwap, now time.Time) (SourceReloadResult, error) {
	if swap == nil {
		return SourceReloadResult{}, nil
	}
	applied, oldDigest, newDigest, rejected, reloadErr := reloader.PollSourceOnce(now)
	result := SourceReloadResult{Applied: applied, OldDigest: oldDigest, NewDigest: newDigest, Rejected: rejected}
	if rejected != "" {
		if rollbackErr := swap.Rollback(); rollbackErr != nil {
			return result, fmt.Errorf("restore last applied config after source rejection: %w", errors.Join(reloadErr, rollbackErr))
		}
		reloader.RefreshRestoredSource(now)
		return result, reloadErr
	}
	if commitErr := swap.Commit(); commitErr != nil {
		return result, errors.Join(reloadErr, commitErr)
	}
	return result, reloadErr
}

// SourceReloader owns validation and publishing of the live definition graph.
type SourceReloader interface {
	PollSourceOnce(time.Time) (bool, string, string, string, error)
	RefreshRestoredSource(time.Time)
}

// ApplyResponse reports one reconcile attempt's outcome. Applied is true only
// when the daemon's live definitions actually changed to the newly pulled
// revision. Rejected carries a config-validation failure message (the
// daemon kept its last-known-good definitions, matching the same-process
// hand-edit reload's existing reject semantics); Error is a distinct
// operational failure (e.g. the git fetch itself failed) that isn't a
// judgment about the pulled config's validity.
type ApplyResponse struct {
	Applied   bool   `json:"applied"`
	OldDigest string `json:"oldDigest,omitempty"`
	NewDigest string `json:"newDigest,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Rejected  string `json:"rejected,omitempty"`
	Error     string `json:"error,omitempty"`
}
