//go:build cloudintegration

package journal_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/journal"
)

type stallEngine struct {
	mu         sync.Mutex
	cancelled  []string
	terminated []string
	cancelErr  error
}

func (e *stallEngine) Cancel(_ context.Context, runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelled = append(e.cancelled, runID)
	return e.cancelErr
}

func (e *stallEngine) Terminate(_ context.Context, runID, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.terminated = append(e.terminated, runID)
	return nil
}

func TestCloudStallUsesLastAppend(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000007816"
	createEngineRun(t, backend, runID, now.Add(-4*time.Hour), now)
	engine := &stallEngine{}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	if result, err := sweeper.Sweep(t.Context()); err != nil || result.Cancelled != 0 {
		t.Fatalf("fresh append sweep = %+v %v", result, err)
	}
	if _, err := config.Pool.Exec(t.Context(), `
		UPDATE journal.run_seq
		SET last_append_at=clock_timestamp()-interval '1 hour'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
	result, err := sweeper.Sweep(t.Context())
	if err != nil || result.Cancelled != 1 {
		t.Fatalf("stale append sweep = %+v %v", result, err)
	}
	if len(engine.cancelled) != 1 || engine.cancelled[0] != runID {
		t.Fatalf("cancelled = %v", engine.cancelled)
	}
	if result, err := sweeper.Sweep(t.Context()); err != nil || result.Cancelled != 0 {
		t.Fatalf("idempotent repeat = %+v %v", result, err)
	}
	if len(engine.cancelled) != 1 {
		t.Fatalf("repeat double-cancelled run: %v", engine.cancelled)
	}
}

func TestCloudMaxRunDurationIgnoresControllerDowntime(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000017816"
	createEngineRun(t, backend, runID, now.Add(-2*time.Hour), now)
	engine := &stallEngine{}
	// A newly constructed controller has no downtime ledger. Cloud execution
	// still spent the two hours on workers, so maxRunDuration expires.
	sweeper := cloudSweeper(t, config, backend, engine, time.Hour, time.Hour)
	result, err := sweeper.Sweep(t.Context())
	if err != nil || result.Cancelled != 1 {
		t.Fatalf("max duration sweep = %+v %v", result, err)
	}
}

func TestCloudStallTerminatesWhenNoWorkerServes(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000027816"
	createEngineRun(t, backend, runID, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err := config.Pool.Exec(t.Context(), `
		UPDATE journal.run_seq
		SET last_append_at=clock_timestamp()-interval '1 hour'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
	engine := &stallEngine{cancelErr: scheduling.ErrNoWorker}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	result, err := sweeper.Sweep(t.Context())
	if err != nil || result.Terminated != 1 {
		t.Fatalf("no-worker sweep = %+v %v", result, err)
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != runID {
		t.Fatalf("terminated = %v", engine.terminated)
	}
}

func cloudSweeper(
	t *testing.T,
	config journalpg.Config,
	backend *journalpg.Backend,
	engine scheduling.EngineControl,
	stallTimeout time.Duration,
	maxDuration time.Duration,
) *scheduling.StallSweeper {
	t.Helper()
	sweeper, err := scheduling.NewStallSweeper(scheduling.StallConfig{
		Pool: config.Pool, InstanceID: config.InstanceID,
		Manager: config.Lease, Handle: config.LiveOwner(),
		Source: backend, Engine: engine,
		StalledTimeout: stallTimeout, MaxRunDuration: maxDuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sweeper
}

func createEngineRun(
	t *testing.T,
	backend *journalpg.Backend,
	runID string,
	startedAt time.Time,
	lastAppendAt time.Time,
) {
	t.Helper()
	handle, owner, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{
		Identity: journal.RunIdentity{
			RunID: runID, Gaggle: "g", Workflow: "implementation",
			Driver: journal.DriverEngine, StartedAt: startedAt,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{
		Type: journal.EventRunnerAnnotation, Time: lastAppendAt,
	}}); err != nil {
		t.Fatal(err)
	}
}

func databaseClock(t *testing.T, config journalpg.Config) time.Time {
	t.Helper()
	var now time.Time
	if err := config.Pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC()
}
