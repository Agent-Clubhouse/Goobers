package childworkflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestRuntimeReloadWaitsForIssuanceThenRevokesBeforePublication(t *testing.T) {
	r, f, env := runtimeFixture(t)
	loading, releaseLoad := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	load := f.resolver.LoadPinnedStage
	f.resolver.LoadPinnedStage = func(ctx context.Context, id journal.RunIdentity, stage string) (PinnedStageAdmission, error) {
		if calls.Add(1) == 1 {
			close(loading)
			select {
			case <-releaseLoad:
			case <-ctx.Done():
				return PinnedStageAdmission{}, ctx.Err()
			}
		}
		return load(ctx, id, stage)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type acquired struct {
		access *mcpio.ChildWorkflowAccess
		close  func() error
		err    error
	}
	issued := make(chan acquired, 1)
	go func() {
		access, closeAccess, err := r.Acquire(ctx, env, journal.NewRegistryScrubber())
		issued <- acquired{access, closeAccess, err}
	}()
	select {
	case <-loading:
	case <-ctx.Done():
		t.Fatal("issuer did not reach pinned loader")
	}
	next := copyRuntimeDefinitions(r.definitions)
	next.Gaggles[0].Annotations = map[string]string{"permission-revision": "next"}
	reloading, published := make(chan struct{}), make(chan struct{})
	reloaded := make(chan error, 1)
	go func() {
		close(reloading)
		err := r.ApplyDefinitions(ctx, next, func() error {
			row, err := r.queue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}, env.ChildWorkflowOrigin.StageOccurrence)
			if err != nil {
				return err
			}
			if !row.Revoked {
				return errors.New("scheduler publication preceded grant revocation")
			}
			close(published)
			return nil
		})
		reloaded <- err
	}()
	<-reloading
	// The pinned lookup is deliberately stalled inside the issuance operation.
	// A publication during this window could miss the not-yet-bound grant.
	select {
	case <-published:
		t.Error("reload passed an in-flight issuer")
	case err := <-reloaded:
		t.Errorf("reload returned before issuance was released: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseLoad)
	var result acquired
	select {
	case result = <-issued:
	case <-ctx.Done():
		t.Fatal("issuer and reload deadlocked")
	}
	if result.err != nil || result.access == nil || result.close == nil {
		t.Fatalf("issuance failed: %v", result.err)
	}
	t.Cleanup(func() { _ = result.close() })
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("reload did not finish after issuance")
	}
	if _, err := r.HTTPService().ValidateChildWorkflow(ctx, result.access.BearerToken, env.RunID, []byte(validProposal)); err == nil {
		t.Fatal("grant minted concurrently with reload survived revocation")
	}
	if _, _, err := r.Acquire(ctx, env, journal.NewRegistryScrubber()); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("revoked active attempt renewed: %v", err)
	}
}
