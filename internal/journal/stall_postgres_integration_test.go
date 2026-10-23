//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/stallsweep"
	"github.com/goobers/goobers/internal/journal"
)

type stallEngine struct {
	mu         sync.Mutex
	cancelled  []string
	terminated []string
	cancelErr  error
}

type scriptedStallEngine struct {
	mu         sync.Mutex
	cancel     func(string) error
	cancelled  []string
	terminated []string
}

func (e *scriptedStallEngine) Cancel(_ context.Context, runID string) error {
	e.mu.Lock()
	e.cancelled = append(e.cancelled, runID)
	fn := e.cancel
	e.mu.Unlock()
	if fn != nil {
		return fn(runID)
	}
	return nil
}

func (e *scriptedStallEngine) Terminate(_ context.Context, runID, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.terminated = append(e.terminated, runID)
	return nil
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
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
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
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run SET created_at=clock_timestamp()-interval '2 hours'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
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
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_seq
		SET last_append_at=clock_timestamp()-interval '1 hour'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
	engine := &stallEngine{cancelErr: stallsweep.ErrNoWorker}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	result, err := sweeper.Sweep(t.Context())
	if err != nil || result.Terminated != 1 {
		t.Fatalf("no-worker sweep = %+v %v", result, err)
	}
	if len(engine.terminated) != 1 || engine.terminated[0] != runID {
		t.Fatalf("terminated = %v", engine.terminated)
	}
	reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: runID, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseEscalated {
		t.Fatalf("terminated journal phase = %q %v", phase, err)
	}
}

func TestCloudStallUsesDatabaseClockForActivity(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	runID := "00000000000000000000000000037816"
	skewed := func() time.Time { return time.Now().Add(-2 * time.Hour) }
	handle, owner, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{
		Identity: journal.RunIdentity{
			RunID: runID, Gaggle: "g", Workflow: "implementation",
			Driver: journal.DriverEngine, StartedAt: skewed(),
		},
		Options: journal.RunWriterOptions{Clock: skewed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AppendBatch(t.Context(), handle, owner,
		[]journal.Event{{Type: journal.EventRunnerAnnotation}}); err != nil {
		t.Fatal(err)
	}
	engine := &stallEngine{}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	result, err := sweeper.Sweep(t.Context())
	if err != nil || result.Cancelled != 0 {
		t.Fatalf("skewed writer looked stale: %+v %v", result, err)
	}
}

func TestCloudStaleSweeperCannotTerminateAfterTakeover(t *testing.T) {
	config := postgresJournalConfigRenew(t, false)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000047816"
	createEngineRun(t, backend, runID, now.Add(-2*time.Hour), now)
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_seq
		SET last_append_at=clock_timestamp()-interval '1 hour'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	engine := &scriptedStallEngine{}
	engine.cancel = func(string) error {
		select {
		case entered <- struct{}{}:
			<-release
			return stallsweep.ErrNoWorker
		default:
			return nil
		}
	}
	old := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	oldDone := make(chan error, 1)
	go func() {
		_, err := old.Sweep(context.Background())
		oldDone <- err
	}()
	<-entered
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE control.lease_heartbeat
		SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND name='controller'`,
		config.InstanceID,
	); err != nil {
		t.Fatal(err)
	}
	successorManager, err := cloudlease.New(cloudlease.Config{
		Pool: config.Pool, InstanceID: config.InstanceID,
		Name: "controller", Holder: "successor",
	})
	if err != nil {
		t.Fatal(err)
	}
	successorHandle, err := successorManager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	successorBackend, err := journalpg.New(journalpg.Config{
		Pool: config.Pool, InstanceID: config.InstanceID,
		Lease:     successorManager,
		LiveOwner: func() *cloudlease.Handle { return successorHandle },
	})
	if err != nil {
		t.Fatal(err)
	}
	successor, err := stallsweep.NewStallSweeper(stallsweep.StallConfig{
		Pool: config.Pool, InstanceID: config.InstanceID,
		Manager: successorManager, Handle: successorHandle,
		Source: backend, Engine: engine, Terminalizer: successorBackend,
		StalledTimeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := successor.Sweep(t.Context()); err != nil || result.Cancelled != 1 {
		t.Fatalf("successor sweep = %+v %v", result, err)
	}
	close(release)
	<-oldDone
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.terminated) != 0 {
		t.Fatalf("stale sweeper terminated after takeover: %v", engine.terminated)
	}
}

func TestCloudFailingRunDoesNotBlockFollowingRun(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	first := "00000000000000000000000000057816"
	second := "00000000000000000000000000067816"
	for _, runID := range []string{first, second} {
		createEngineRun(t, backend, runID, now.Add(-2*time.Hour), now)
	}
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_seq
		SET last_append_at=clock_timestamp()-interval '1 hour'
		WHERE instance_id=$1`,
		config.InstanceID,
	); err != nil {
		t.Fatal(err)
	}
	engine := &scriptedStallEngine{cancel: func(runID string) error {
		if runID == first {
			return errors.New("workflow execution not found")
		}
		return nil
	}}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	result, err := sweeper.Sweep(t.Context())
	if err == nil || result.Cancelled != 1 {
		t.Fatalf("isolated sweep = %+v %v", result, err)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	found := false
	for _, runID := range engine.cancelled {
		found = found || runID == second
	}
	if !found {
		t.Fatalf("failing first run blocked second: %v", engine.cancelled)
	}
}

func TestCloudFailedTerminalCleanupIsRetried(t *testing.T) {
	config := postgresJournalConfig(t)
	var (
		cleanupMu sync.Mutex
		calls     int
	)
	config.TerminalCleanup = func(
		context.Context,
		journal.RunIdentity,
		string,
	) error {
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("forge cleanup unavailable")
		}
		return nil
	}
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000097816"
	createEngineRun(t, backend, runID, now.Add(-2*time.Hour), now)
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_seq
		SET last_append_at=clock_timestamp()-interval '1 hour'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
	sweeper := cloudSweeper(t, config, backend,
		&stallEngine{cancelErr: stallsweep.ErrNoWorker}, 30*time.Minute, 0)
	_, _ = sweeper.Sweep(t.Context())
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE control.stall_action
		SET retry_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	); err != nil {
		t.Fatal(err)
	}
	if result, err := sweeper.Sweep(t.Context()); err != nil ||
		result.Terminated != 1 {
		t.Fatalf("cleanup retry sweep = %+v %v", result, err)
	}
	var state string
	if err := config.Pool.QueryRow(t.Context(), `
		SELECT state FROM control.stall_action
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	if calls < 2 || state != "terminated" {
		t.Fatalf("cleanup calls=%d action state=%q", calls, state)
	}
}

func cloudSweeper(
	t *testing.T,
	config journalpg.Config,
	backend *journalpg.Backend,
	engine stallsweep.EngineControl,
	stallTimeout time.Duration,
	maxDuration time.Duration,
) *stallsweep.StallSweeper {
	t.Helper()
	sweeper, err := stallsweep.NewStallSweeper(stallsweep.StallConfig{
		Pool: config.Pool, InstanceID: config.InstanceID,
		Manager: config.Lease, Handle: config.LiveOwner(),
		Source: backend, Engine: engine, Terminalizer: backend,
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
