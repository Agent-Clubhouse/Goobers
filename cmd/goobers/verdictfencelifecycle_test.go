package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestReviewPublicationQuarantineSurvivesTerminalCleanup(t *testing.T) {
	for _, mode := range []string{"file", "plane"} {
		t.Run(mode, func(t *testing.T) {
			plane := newClaimsPlane(t)
			local, err := fileClaimLedger(plane.layout)
			if err != nil {
				t.Fatal(err)
			}
			token := plane.admitRun(t, "test", "first")
			ledger := local
			if mode == "plane" {
				ledger = plane.client(t, "first", token)
			}
			lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
			err = withPublicationClaim(t.Context(), ledger, lock, time.Second, 20*time.Millisecond, time.Minute, func(ctx context.Context) error { <-ctx.Done(); return nil })
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			released, err := ledger.ReleaseAllForRun(t.Context(), "first", claimrelease.Completed.Reason())
			if err != nil || len(released) != 0 {
				t.Fatalf("bulk release reported custody released: %+v %v", released, err)
			}
			run, _, err := journal.Recover(filepath.Join(plane.layout.ForGaggle("test").RunsDir(), "first"))
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			if err = releaseClaimsForRun(plane.layout, nil, "first"); err != nil {
				t.Fatal(err)
			}
			if released, err := recoverClaims(plane.layout, nil, time.Now(), nil, nil); err != nil || len(released) != 0 {
				t.Fatalf("terminal recovery released custody: %+v %v", released, err)
			}
			ok, holder, err := local.ClaimScoped(t.Context(), lock.Key, "second", "merge-review", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if ok || holder != "first" {
				t.Fatalf("terminal cleanup erased uncertain-action quarantine: new owner acquired=%t holder=%q", ok, holder)
			}
		})
	}
}

func TestReviewPublicationQuarantineNotExtendedByLiveRenewal(t *testing.T) {
	plane := newClaimsPlane(t)
	ledger, err := fileClaimLedger(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
	err = withPublicationClaim(t.Context(), ledger, lock, time.Second, 20*time.Millisecond, time.Minute, func(ctx context.Context) error { <-ctx.Done(); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	before, err := ledger.ForRunAll(t.Context(), "first")
	if err != nil || len(before) != 1 {
		t.Fatalf("before %v %v", before, err)
	}
	durable, err := localscheduler.OpenClaimLedger(filepath.Join(plane.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = renewCoordinatedLiveClaims(t.Context(), plane.layout, durable, map[string]bool{"first": true}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ledger.ForRunAll(t.Context(), "first")
	if err != nil || len(after) != 1 {
		t.Fatalf("after %v %v", after, err)
	}
	if !after[0].ExpiresAt.Equal(before[0].ExpiresAt) {
		t.Fatalf("routine renewal changed fixed quarantine expiry from %s to %s", before[0].ExpiresAt, after[0].ExpiresAt)
	}
}
