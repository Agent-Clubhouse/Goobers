package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

type lockCheckingSharedStore struct {
	pinnedClaimTestStore
	check   func() error
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *lockCheckingSharedStore) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	if err := s.check(); err != nil {
		return sharedclaim.Observation{}, err
	}
	if s.entered != nil {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		})
	}
	return s.pinnedClaimTestStore.Read(ctx, key)
}
func (s *lockCheckingSharedStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.pinnedClaimTestStore.CompareAndSwap(ctx, key, revision, record)
}

func TestSharedProviderIOOutsideDaemonLifecycleAndBacklogLocks(t *testing.T) {
	for _, path := range []string{"daemon-acquire", "daemon-renew", "daemon-release", "lifecycle-recovery", "backlog-selection"} {
		t.Run(path, func(t *testing.T) {
			layout, run := newPinnedClaimResolverRun(t, "shared")
			lockPath := filepath.Join(layout.SchedulerDir(), claimLockFileName)
			store := &lockCheckingSharedStore{check: func() error {
				if err := withClaimLockBounds(lockPath, "test.provider-lock-probe", 100*time.Millisecond, time.Second, claimLockEventContext{}, func() error { return nil }); err != nil {
					return fmt.Errorf("provider I/O entered while claims lock held: %w", err)
				}
				return nil
			}}
			resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) { return store, nil }}
			service := newDaemonClaimService(layout, nil, nil)
			service.shared = resolver
			request := httpapi.ClaimRequest{Gaggle: "example", Provider: "github", ItemID: "42", RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60}
			if path != "daemon-acquire" && path != "backlog-selection" {
				if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
					t.Fatalf("seed: %+v %v", response, err)
				}
			}
			var execute func() error
			switch path {
			case "daemon-acquire":
				execute = func() error {
					response, err := service.Acquire(t.Context(), request)
					if err == nil && !response.Ok {
						return fmt.Errorf("admission refused")
					}
					return err
				}
			case "daemon-renew":
				execute = func() error {
					response, err := service.Renew(t.Context(), request)
					if err == nil && !response.Ok {
						return fmt.Errorf("renewal refused")
					}
					return err
				}
			case "daemon-release":
				execute = func() error {
					bulk := request
					bulk.ItemID = ""
					_, err := service.Release(t.Context(), bulk)
					return err
				}
			case "lifecycle-recovery":
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
					t.Fatal(err)
				}
				execute = func() error {
					_, err := recoverClaimsWithResolver(layout, nil, time.Now(), nil, nil, resolver)
					return err
				}
			case "backlog-selection":
				ledger, err := fileClaimLedgerWithResolver(layout, resolver)
				if err != nil {
					t.Fatal(err)
				}
				annotator, err := openStageAnnotator(layout)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = annotator.Close() }()
				repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
				session := &backlogClaimSession{ledger: ledger, annotations: annotator, runID: "shared-run", workflow: "claim", gaggle: "example", leaseDuration: time.Minute, maxItems: 1,
					env: backlogQueryEnv{layout: layout, repo: repo, backlogRepo: repo}, eligible: []providers.WorkItem{{ID: "42"}}, claimSetPrepared: true, driftBackoffChecked: true, preexistingClaimIDs: map[string]struct{}{}}
				execute = func() error {
					if err := session.acquire(t.Context()); err != nil {
						return err
					}
					if len(session.claimed) != 1 {
						return fmt.Errorf("backlog failed to claim")
					}
					return nil
				}
			}
			store.entered, store.resume = make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(store.resume) })
			result := make(chan error, 1)
			go func() { result <- execute() }()
			select {
			case <-store.entered:
			case err := <-result:
				t.Fatalf("failed before blocked provider: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("provider never entered")
			}
			// An unrelated claimant must succeed while the network remains blocked.
			local, err := fileClaimLedger(layout)
			if err != nil {
				t.Fatal(err)
			}
			other := make(chan error, 1)
			go func() {
				ok, _, err := local.ClaimScoped(t.Context(), claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "99"}, "other", "work", time.Minute)
				if err == nil && !ok {
					err = fmt.Errorf("unrelated claimant refused")
				}
				other <- err
			}()
			select {
			case err := <-other:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked provider retained claims lock")
			}
			unblock.Do(func() { close(store.resume) })
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shared transition did not finish")
			}
		})
	}
}
