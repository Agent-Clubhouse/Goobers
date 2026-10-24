//go:build cloudintegration

package terminalwriter

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/claims"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// Exercise the real custody -> pinned writer -> provider -> history path as
// goobers_controller. Provider failures retain custody until a successful retry.
func TestCleanupCustodyUsesThePinnedProviderWriter(t *testing.T) {
	backend, admin, controller, manager, handle := controllerJournalLease(t)
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}
	const runID = "run-cleanup"
	if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{
		RunID: runID, Gaggle: "core", Workflow: "work", ConfigGeneration: "gen-1", WorkspaceRepository: &project}}); err != nil {
		t.Fatal(err)
	}
	repo, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `INSERT INTO control.run_pin
		(instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
		 goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch)
		VALUES ('test-instance',$1,'acc-1','core','work',1,'wd','gd','gen-1',$2,'','id',$3)`, runID, repo, handle.Epoch); err != nil {
		t.Fatal(err)
	}
	pins, err := acceptance.New(controller, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "core"}, Spec: apiv1.GaggleSpec{
		Project: project, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/service"},
	}}}}
	runs := JournalRuns{
		Pins: AcceptancePins(pins.RunPinsOf), Children: AcceptanceChildren(pins.ChildForExecutionRun),
		Journal: backend, Generations: pinnedGenerations{"gen-1": set},
	}
	provider := &fakeBackend{calls: &[]call{}, err: errors.New("provider temporarily unavailable")}
	writer, err := New(runs, fakeBackends{provider}, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	plane, err := claims.NewPlane(claims.PlaneConfig{Pool: controller, InstanceID: "test-instance", Lease: manager,
		Owner: func() *lease.Handle { return handle }, DefaultLease: time.Minute, ReleaseMarker: writer.ReleaseClaim})
	if err != nil {
		t.Fatal(err)
	}
	req := httpapi.ClaimRequest{RunID: runID, Gaggle: "core", Workflow: "work", Provider: "github", ItemID: "42"}
	if _, err := plane.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	list := func() httpapi.ClaimListResponse {
		t.Helper()
		got, err := plane.List(t.Context(), httpapi.ClaimListRequest{RunID: runID, Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := list()
	if len(before.Entries) != 1 {
		t.Fatalf("claims = %+v", before)
	}
	req.ProviderCleanup, req.ClaimedAt = true, before.Entries[0].ClaimedAt
	req.Reason = claimrelease.Abandoned.Because("backlog_query_failed", "")
	for _, tc := range []struct {
		err  error
		code string
	}{
		{errors.New("provider temporarily unavailable"), "provider_cleanup_failed"},
		{&providers.ClaimHeldError{Holder: "successor", Expected: runID}, "provider_cleanup_refused"},
	} {
		provider.err = tc.err
		_, err := plane.Release(t.Context(), req)
		var refused *httpapi.InterventionError
		if !errors.As(err, &refused) || refused.Code != tc.code {
			t.Fatalf("cleanup = %v, want %s", err, tc.code)
		}
		if held := list(); len(held.Entries) != 1 || !held.Entries[0].ReleasePending {
			t.Fatalf("custody lost: %+v", held)
		}
	}
	provider.err = nil
	if got, err := plane.Release(t.Context(), req); err != nil || !got.Ok {
		t.Fatalf("retry = %+v, %v", got, err)
	}
	if got := provider.released; got == nil || got.Repository != repositoryRef(project) || got.RunID != runID || got.ExpectedClaimRunID != runID || got.ID != "42" {
		t.Fatalf("provider request = %+v", got)
	}
	if after := list(); len(after.Entries) != 0 || len(after.History) != 1 || after.History[0].ReleasePending || after.History[0].ReleaseReason.Code != req.Reason.Code {
		t.Fatalf("history = %+v", after)
	}
}
