//go:build cloudintegration

package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// TestControlJournalWritesAsTheControllerRoleAndLeavesNoLocalFiles composes the
// journal plane exactly as the control role does, over the real controller
// login role, and proves an emit lands in journal.* with no local file.
func TestControlJournalWritesAsTheControllerRoleAndLeavesNoLocalFiles(t *testing.T) {
	instance, apiURL, controllerURL := controlRoleDatabase(t)
	controllerConfig, err := pgxpool.ParseConfig(controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	controllerConfig.MaxConns = lease.MaxFencedConnections
	controller, err := pgxpool.NewWithConfig(t.Context(), controllerConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	var role string
	if err := controller.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("controller pool authenticates as %q (%v)", role, err)
	}
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: instance, Name: lease.ControllerName, Holder: "journal-assembly", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	handle := pgtest.Acquire(t, manager)
	t.Cleanup(func() { _ = manager.Release(t.Context(), handle) })
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var planes controlPlanes
	db := databaseServices{Pools: &cloudpg.Pools{Transaction: controller, Direct: controller}, InstanceID: instance, Blobs: blobs}
	if err := constructJournal(t.Context(), db, controlAuth{}, manager, func() *lease.Handle { return handle }, &planes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(planes.close)

	runID := strings.Repeat("a", 32)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	started, stage := journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}, journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}
	response, err := planes.Journal.Emit(t.Context(), livejournal.EmitRequest{
		RunID: runID, Gaggle: "web",
		Open: &livejournal.OpenHeader{
			Identity: journal.RunIdentity{RunID: runID, Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: "sha256:abc", Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}},
			Graph:    []byte(`{"nodes":[]}`), Definition: []byte(`{"name":"wf"}`),
		},
		Ops: []livejournal.Op{
			{Kind: livejournal.OpAppend, Key: runID + "|0||0|0", Event: &started, Time: at},
			{Kind: livejournal.OpAppend, Key: runID + "|0|build|1|0", Event: &stage, Time: at.Add(time.Second)},
		},
	})
	if err != nil || response.Seq != 2 {
		t.Fatalf("emit = %+v, %v", response, err)
	}
	var events int
	if err := controller.QueryRow(t.Context(), "SELECT count(*) FROM journal.run_event WHERE instance_id=$1 AND run_id=$2", instance, runID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("journal.run_event rows = %d, %v", events, err)
	}
	if _, err := os.Stat(cloudJournalRunsRoot); !os.IsNotExist(err) {
		t.Fatalf("PG journal touched the local runs root: %v", err)
	}

	// The API login role has no journal grants in P3 (writepaths.yaml): a read
	// through its pool must be refused, so nothing may route journal reads there.
	api, err := pgxpool.New(t.Context(), apiURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	reader, err := journalpg.New(journalpg.Config{Pool: api, InstanceID: instance})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.OpenRun(t.Context(), journal.RunRef{ID: runID, ReadOnly: true}); err == nil {
		t.Fatal("API role read the journal; journal reads must use the controller pool")
	}
}
