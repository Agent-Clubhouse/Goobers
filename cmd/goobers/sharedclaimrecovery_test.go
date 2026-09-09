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
