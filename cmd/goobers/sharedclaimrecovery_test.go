package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

type keyedRecoveryStore map[string]*failingReleaseClaimStore

func (s keyedRecoveryStore) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	return s[key].Read(ctx, key)
}

func (s keyedRecoveryStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	return s[key].CompareAndSwap(ctx, key, revision, record)
}

func TestRecoverySharedFailureDoesNotStarveIndependentClaims(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "expired"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			layout, run := newPinnedClaimResolverRun(t, "shared")
			store := keyedRecoveryStore{"41": {}, "42": {}, "43": {}}
			resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
				return store, nil
			}}
			service := newDaemonClaimService(layout, nil, nil)
			service.shared = resolver
			for _, id := range []string{"41", "42", "43"} {
				request := httpapi.ClaimRequest{Gaggle: "example", Provider: "github", ItemID: id, RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60}
				if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
					t.Fatalf("acquire %s: %+v %v", id, response, err)
				}
			}
			store["41"].failRelease = true
			store["43"].failRelease = true
			now := time.Now().Add(2 * time.Minute)
			if terminal {
				now = time.Now()
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
			}
			released, err := recoverClaimsWithResolver(layout, nil, now, nil, nil, resolver)
			if err == nil || len(released) != 1 || released[0].ExternalID != "42" {
				t.Fatalf("partial recovery: %+v %v", released, err)
			}
			if joined, ok := err.(interface{ Unwrap() []error }); !ok || len(joined.Unwrap()) < 2 {
				t.Fatalf("cleanup failures not accumulated: %v", err)
			}
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			if entries := ledger.Snapshot(); len(entries) != 2 {
				t.Fatalf("failed cleanup custody lost: %+v", entries)
			}
			store["41"].failRelease = false
			store["43"].failRelease = false
			if released, err := recoverClaimsWithResolver(layout, nil, now, nil, nil, resolver); err != nil || len(released) != 2 {
				t.Fatalf("retry: %+v %v", released, err)
			}
		})
	}
}

func TestRecoverySharedCleanupRequiresProviderAcknowledgement(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "expired"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			layout, run := newPinnedClaimResolverRun(t, "shared")
			store := &failingReleaseClaimStore{}
			resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
				return store, nil
			}}
			service := newDaemonClaimService(layout, nil, nil)
			service.shared = resolver
			request := httpapi.ClaimRequest{Gaggle: "example", Provider: "github", ItemID: "42", RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60}
			if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
				t.Fatalf("acquire: %+v %v", response, err)
			}
			now := time.Now().Add(2 * time.Minute)
			if terminal {
				now = time.Now()
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
			}
			store.failRelease = true
			if released, err := recoverClaimsWithResolver(layout, nil, now, nil, nil, resolver); err == nil || len(released) != 0 {
				t.Fatalf("unacknowledged recovery: %+v %v", released, err)
			}
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			entry, held := ledger.LookupScoped(claimKey(request))
			if !held || entry.SharedOwner != store.record.Owner || entry.SharedDeadline.IsZero() {
				t.Fatalf("failed recovery discarded ownership: %+v", entry)
			}
			store.failRelease = false
			if released, err := recoverClaimsWithResolver(layout, nil, now, nil, nil, resolver); err != nil || len(released) != 1 || store.record.Owner != (sharedclaim.Owner{}) {
				t.Fatalf("acknowledged recovery: %+v %v", released, err)
			}
			writes := store.writes
			if released, err := recoverClaimsWithResolver(layout, nil, now, nil, nil, resolver); err != nil || len(released) != 0 || store.writes != writes {
				t.Fatalf("duplicate recovery mutated provider: %+v %v", released, err)
			}
		})
	}
}
