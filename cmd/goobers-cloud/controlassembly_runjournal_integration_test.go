//go:build cloudintegration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workflow"
)

// unusedReader stands in for the cloud Reader on routes that never reach it.
type unusedReader struct{ readservice.Reader }

// TestControlRunJournalAnswersFromTheControllerStores composes the run-journal
// read plane over the journal and claims planes as goobers_controller and
// checks its containment and its claims-derived stranded-work answer.
func TestControlRunJournalAnswersFromTheControllerStores(t *testing.T) {
	instanceID, _, controllerURL := controlRoleDatabase(t)
	config, err := pgxpool.ParseConfig(controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = lease.MaxFencedConnections
	controller, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	var role string
	if err := controller.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("controller pool authenticates as %q (%v)", role, err)
	}
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: instanceID, Name: lease.ControllerName, Holder: "run-journal", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handle := pgtest.Acquire(t, manager)
	t.Cleanup(func() { _ = manager.Release(context.Background(), handle) })
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner := func() *lease.Handle { return handle }
	db := databaseServices{Pools: &cloudpg.Pools{Transaction: controller, Direct: controller}, InstanceID: instanceID, Blobs: blobs}
	db.Acceptances = &acceptance.Store{}
	planes := claimProviderPlanes()
	for _, construct := range []func(context.Context, databaseServices, controlAuth, *lease.Manager, func() *lease.Handle, *controlPlanes) error{constructJournal, constructClaims} {
		if err := construct(t.Context(), db, controlAuth{}, manager, owner, &planes); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(planes.close)

	asking, sibling, foreign := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	for _, run := range []struct{ id, gaggle string }{{asking, "web"}, {sibling, "web"}, {foreign, "api"}} {
		emitRun(t, planes.Journal, run.id, run.gaggle)
	}
	if _, err := planes.Claims.Acquire(t.Context(), httpapi.ClaimRequest{Gaggle: "web", Provider: "github", ItemID: "42", RunID: asking, Workflow: "wf"}); err != nil {
		t.Fatalf("acquire the asking run's claim: %v", err)
	}

	// The stranded-work answer must come from the claims the run holds: spy on
	// the real lookup.
	var mu sync.Mutex
	var looked []string
	real := planes.claimedItems
	planes.claimedItems = func(ctx context.Context, runID string) ([]string, error) {
		ids, err := real(ctx, runID)
		mu.Lock()
		looked = append(looked, ids...)
		mu.Unlock()
		return ids, err
	}
	planes.Reader = unusedReader{}
	applied := false
	planes.appliedSet = func() (*instance.ConfigSet, bool) {
		return &instance.ConfigSet{Workflows: []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Gaggle: "web"}}}}, applied
	}
	if err := constructRunJournal(t.Context(), db, controlAuth{}, manager, owner, &planes); err != nil {
		t.Fatal(err)
	}
	service := planes.RunJournal

	phase, err := service.RunPhase(t.Context(), journalclient.RunPhaseRequest{RunID: asking, Gaggle: "web", TargetRunID: sibling})
	if err != nil || phase.Phase != string(journal.PhaseRunning) {
		t.Fatalf("same-gaggle run phase = %+v, %v", phase, err)
	}
	var refused *httpapi.InterventionError
	if _, err := service.RunPhase(t.Context(), journalclient.RunPhaseRequest{RunID: asking, Gaggle: "web", TargetRunID: foreign}); !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Fatalf("another gaggle's run phase: %v, want a 403 gaggle mismatch", err)
	}

	// Windowed routes refuse until a configuration generation is applied.
	since := time.Now().Add(-time.Hour)
	if _, err := service.ConflictTouches(t.Context(), journalclient.ConflictTouchRequest{RunID: asking, Gaggle: "web", Since: since}); err == nil || !strings.Contains(err.Error(), "no configuration generation is applied") {
		t.Fatalf("a windowed read before any configuration was applied = %v, want the unapplied refusal", err)
	}
	applied = true
	if _, err := service.UnpushedWork(t.Context(), journalclient.UnpushedWorkRequest{RunID: asking, Gaggle: "web", Since: since}); err != nil {
		t.Fatalf("unpushed work: %v", err)
	}
	mu.Lock()
	gotLooked := append([]string(nil), looked...)
	mu.Unlock()
	if len(gotLooked) != 1 || gotLooked[0] != "42" {
		t.Fatalf("stranded work consulted claims %v, want the run's own [42]", gotLooked)
	}

	// A claims read that fails must fail the answer, never read as "no claims".
	lookupFailed := errors.New("claims unavailable")
	planes.claimedItems = func(context.Context, string) ([]string, error) { return nil, lookupFailed }
	if err := constructRunJournal(t.Context(), db, controlAuth{}, manager, owner, &planes); err != nil {
		t.Fatal(err)
	}
	if _, err := planes.RunJournal.UnpushedWork(t.Context(), journalclient.UnpushedWorkRequest{RunID: asking, Gaggle: "web", Since: since}); !errors.Is(err, lookupFailed) {
		t.Fatalf("unpushed work with a failing claims read = %v, want the failure", err)
	}
}

func emitRun(t *testing.T, service httpapi.JournalService, runID, gaggle string) {
	t.Helper()
	graph, err := json.Marshal(workflow.Graph{Name: "wf", Version: 1, Digest: "sha256:abc"})
	if err != nil {
		t.Fatal(err)
	}
	started := journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}
	if _, err := service.Emit(t.Context(), livejournal.EmitRequest{
		RunID: runID, Gaggle: gaggle,
		Open: &livejournal.OpenHeader{
			Identity: journal.RunIdentity{RunID: runID, Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Gaggle: gaggle, Trigger: journal.Trigger{Kind: journal.TriggerManual}},
			Graph:    graph, Definition: []byte(`{"name":"wf"}`),
		},
		Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: runID + "|0||0|0", Event: &started, Time: time.Now()}},
	}); err != nil {
		t.Fatalf("emit run %s: %v", runID, err)
	}
}
