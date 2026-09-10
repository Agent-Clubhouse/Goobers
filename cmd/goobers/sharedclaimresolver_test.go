package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

type pinnedClaimTestStore struct {
	record   sharedclaim.Record
	revision string
	writes   int
}

func TestPinnedResolverReconciliationUsesOwnerPolicyAndDistinctIncarnation(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return &pinnedClaimTestStore{}, nil
	}}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	ordinary, err := resolver.Admission(t.Context(), key, "shared-run", "claim")
	if err != nil {
		t.Fatal(err)
	}
	reservationID := formatBacklogReconcileRunID("shared-run", 42, 1)
	reservation, err := resolver.Admission(t.Context(), key, reservationID, "backlog-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if reservation.RemoteKey != ordinary.RemoteKey || reservation.Owner == ordinary.Owner {
		t.Fatal("reservation must compete for the same remote key with a distinct owner")
	}
	second, err := resolver.Admission(t.Context(), key, formatBacklogReconcileRunID("shared-run", 42, 2), "backlog-reconcile")
	if err != nil || second.Owner == reservation.Owner {
		t.Fatalf("concurrent reservation shares ownership: %v", err)
	}
	for _, tc := range []struct{ run, workflow string }{
		{reservationID, "claim"},
		{formatBacklogReconcileRunID("missing", 42, 1), "backlog-reconcile"},
		{"shared-run/backlog-reconcile/42/1/extra", "backlog-reconcile"},
	} {
		if _, err := resolver.Admission(t.Context(), key, tc.run, tc.workflow); err == nil {
			t.Fatalf("unverified reservation accepted: %+v", tc)
		}
	}
}

func (s *pinnedClaimTestStore) Read(context.Context, string) (sharedclaim.Observation, error) {
	return sharedclaim.Observation{Record: s.record, Revision: s.revision, Now: time.Now()}, nil
}

func TestReconciliationSharedLeaseCannotReleaseOrdinaryClaim(t *testing.T) {
	layout, run := newPinnedClaimResolverRun(t, "shared")
	store := &pinnedClaimTestStore{}
	resolver := stageClaimResolver{pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return store, nil
	}}}
	file, err := claimsclient.NewFile(claimsclient.FileConfig{LedgerPath: filepath.Join(layout.SchedulerDir(), "claims.json"), Shared: resolver})
	if err != nil {
		t.Fatal(err)
	}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	reservationID := formatBacklogReconcileRunID("shared-run", 42, 1)
	if ok, _, err := file.ClaimScoped(t.Context(), key, reservationID, "backlog-reconcile", time.Minute); err != nil || !ok {
		t.Fatalf("reserve: %t %v", ok, err)
	}
	entries, err := file.ForRunAll(t.Context(), reservationID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("reservation entries: %+v %v", entries, err)
	}
	forged := entries[0]
	forged.RunID, forged.Workflow = "shared-run", "claim"
	if _, err := resolver.Release(t.Context(), forged); err == nil {
		t.Fatal("reservation ownership authorized an ordinary claim release")
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := file.ClaimScoped(t.Context(), key, reservationID, "backlog-reconcile", time.Minute); err == nil || ok {
		t.Fatal("terminal owning run admitted a reservation")
	}
	if released, err := file.ReleaseAllForRun(t.Context(), reservationID); err != nil || len(released) != 1 || store.record.Owner != (sharedclaim.Owner{}) {
		t.Fatalf("terminal reservation release: %+v %v", released, err)
	}
}

func TestStageClaimResolverLocalFallbackDoesNotOverrideRunPin(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			layout, _ := newPinnedClaimResolverRun(t, mode)
			resolver := stageClaimResolver{pinnedSharedClaimResolver{layout: layout}}
			key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
			// The current demo configuration is local. An existing shared pin
			// must still require its provider, while a local pin needs none.
			binding, err := resolver.Admission(t.Context(), key, "shared-run", "claim")
			if mode == "shared" && err == nil {
				t.Fatal("current local configuration overrode a shared pin")
			}
			if mode == "local" && (err != nil || binding != nil) {
				t.Fatalf("local pin needed a provider: %+v %v", binding, err)
			}
			for _, runID := range []string{"legacy-run", formatBacklogReconcileRunID("legacy-run", 42, 1)} {
				workflowName := "claim"
				if runID != "legacy-run" {
					workflowName = "backlog-reconcile"
				}
				if binding, err := resolver.Admission(t.Context(), key, runID, workflowName); err != nil || binding != nil {
					t.Fatalf("legacy local admission %q: %+v %v", runID, binding, err)
				}
			}
		})
	}
}
func (s *pinnedClaimTestStore) CompareAndSwap(_ context.Context, _, revision string, record sharedclaim.Record) error {
	if revision != s.revision {
		return sharedclaim.ErrConflict
	}
	s.writes++
	s.record, s.revision = record, fmt.Sprint(s.writes)
	return nil
}

func newPinnedClaimResolverRun(t *testing.T, mode string) (instance.Layout, *journal.Run) {
	t.Helper()
	layout := instance.NewLayout(initDemo(t))
	definition := workflow.Definition{Name: "claim", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle: "example", Start: "claim", Readiness: apiv1.ReadinessConditions{ClaimVisibility: mode},
		Tasks: []apiv1.Task{{Name: "claim", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
	}}
	machine, err := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "shared-run", Workflow: "claim", WorkflowVersion: 1, Gaggle: "example", WorkflowDigest: machine.Digest(),
		WorkspaceRepository: &apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "pinned-owner", Name: "pinned-repo", Branch: "main"},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: data}, journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return layout, run
}

func TestPinnedResolverCoordinatesFileClaimAndTerminalRelease(t *testing.T) {
	layout, run := newPinnedClaimResolverRun(t, "shared")
	store := &pinnedClaimTestStore{}
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(_ context.Context, repo providers.RepositoryRef) (sharedclaim.Store, error) {
		// initDemo's current config names another repository. Only the run's
		// persisted repository is an acceptable source for remote coordination.
		if repo.Owner != "pinned-owner" || repo.Name != "pinned-repo" || repo.Provider != providers.ProviderGitHub {
			t.Fatalf("provider factory received an unpinned repository: %+v", repo)
		}
		return store, nil
	}}
	file, err := claimsclient.NewFile(claimsclient.FileConfig{LedgerPath: filepath.Join(layout.SchedulerDir(), "claims.json"), Shared: resolver})
	if err != nil {
		t.Fatal(err)
	}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	if ok, _, err := file.ClaimScoped(t.Context(), key, "shared-run", "claim", time.Minute); err != nil || !ok {
		t.Fatalf("admission: %t %v", ok, err)
	}
	entries, err := file.ForRunAll(t.Context(), "shared-run")
	if err != nil || len(entries) != 1 {
		t.Fatalf("persisted claims: %+v %v", entries, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := file.ClaimScoped(t.Context(), key, "shared-run", "claim", time.Minute); err == nil || ok || store.writes != 1 {
		t.Fatal("terminal run renewed shared admission")
	}
	forged := entries[0]
	forged.SharedOwner.Token = "another-incarnation"
	if _, err := resolver.Release(t.Context(), forged); err == nil || store.writes != 1 {
		t.Fatal("forged release owner accepted")
	}
	if released, err := file.ReleaseAllForRun(t.Context(), "shared-run"); err != nil || len(released) != 1 || store.record.Owner != (sharedclaim.Owner{}) {
		t.Fatalf("terminal owner release failed: %+v %v", released, err)
	}
}

func TestPinnedResolverRefusesUnverifiedIdentityBeforeProviderConstruction(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			layout, _ := newPinnedClaimResolverRun(t, mode)
			calls := 0
			resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
				calls++
				return &pinnedClaimTestStore{}, nil
			}}
			for _, tc := range []struct{ run, workflow, gaggle string }{
				{"missing", "claim", "example"}, {"shared-run", "other", "example"}, {"shared-run", "claim", "other"},
			} {
				key := claimsclient.Key{Gaggle: tc.gaggle, Provider: "github", ExternalID: "42"}
				if _, err := resolver.Admission(t.Context(), key, tc.run, tc.workflow); err == nil {
					t.Fatalf("unverified request accepted: %+v", tc)
				}
			}
			if calls != 0 {
				t.Fatal("unverified identity reached provider factory")
			}
			key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
			binding, err := resolver.Admission(t.Context(), key, "shared-run", "claim")
			if err != nil || (binding == nil) != (mode == "local") {
				t.Fatalf("pinned mode: %+v %v", binding, err)
			}
			if mode == "local" && calls != 0 {
				t.Fatal("local policy constructed a remote coordinator")
			}
		})
	}
}

func TestPinnedSharedClaimRecordKeyIsRepositoryRelative(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return &pinnedClaimTestStore{}, nil
	}}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	_, identity, _, err := resolver.claimPolicy(key, "shared-run", "claim")
	if err != nil {
		t.Fatal(err)
	}
	for _, apiURL := range []string{"", "https://api.github.com", "https://api.github.com/"} {
		copy := *identity.WorkspaceRepository
		copy.BaseURL = apiURL
		identity.WorkspaceRepository = &copy
		binding, err := resolver.binding(t.Context(), key, identity, sharedclaim.Owner{})
		if err != nil || binding.RemoteKey != "42" {
			t.Fatalf("equivalent repository spelling split the claim key: %q %+v %v", apiURL, binding, err)
		}
	}
}
