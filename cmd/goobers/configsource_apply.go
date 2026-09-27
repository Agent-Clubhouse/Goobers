package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

type workflowSourceApplier struct {
	mu        sync.Mutex
	root      string
	source    instance.WorkflowSource
	appTokens instance.GitTokenSource
	setup     *schedulerSetup
	reloader  *configReloader
	revision  string
}

func (a *workflowSourceApplier) Apply(ctx context.Context, now time.Time) applyResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	result, revision, _, err := a.sync(ctx, "", now)
	response := applyResponse{
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

func (a *workflowSourceApplier) Reconcile(ctx context.Context, now time.Time) error {
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

func (a *workflowSourceApplier) sync(ctx context.Context, currentRevision string, now time.Time) (sourceReloadResult, string, bool, error) {
	revision, changed, _, swap, err := instance.PrepareGitWorkflowSourceIfChanged(
		ctx, a.root, a.source, currentRevision, a.appTokens, a.setup.SharedRegistry, a.setup.SecretStores,
	)
	if err != nil {
		return sourceReloadResult{}, "", false, fmt.Errorf("sync workflow source: %w", err)
	}
	if !changed {
		return sourceReloadResult{}, revision, false, nil
	}
	result, err := applyPreparedConfigSource(a.reloader, swap, now)
	return result, revision, true, err
}

type sourceReloadResult struct {
	Applied              bool
	OldDigest, NewDigest string
	Rejected             string
}

// applyPreparedConfigSource closes the filesystem/apply transaction. A
// validation rejection restores the last applied tree while preserving the
// reloader's rejected-generation status and journal event. Other apply errors
// retain the candidate, matching the pre-existing explicit divergence state:
// they may have occurred after a non-transactional runtime side effect.
func applyPreparedConfigSource(reloader *configReloader, swap *instance.PreparedConfigSwap, now time.Time) (sourceReloadResult, error) {
	if swap == nil {
		return sourceReloadResult{}, nil
	}
	applied, oldDigest, newDigest, rejected, reloadErr := reloader.pollSourceOnce(now)
	result := sourceReloadResult{Applied: applied, OldDigest: oldDigest, NewDigest: newDigest, Rejected: rejected}
	if rejected != "" {
		if rollbackErr := swap.Rollback(); rollbackErr != nil {
			return result, fmt.Errorf("restore last applied config after source rejection: %w", errors.Join(reloadErr, rollbackErr))
		}
		reloader.refreshRestoredSource(now)
		return result, reloadErr
	}
	if commitErr := swap.Commit(); commitErr != nil {
		return result, errors.Join(reloadErr, commitErr)
	}
	return result, reloadErr
}
