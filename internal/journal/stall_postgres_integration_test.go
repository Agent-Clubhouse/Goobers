//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
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
		Source: backend, Events: stallEvents(backend), Engine: engine, Terminalizer: successorBackend,
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
	report ...func(error),
) *stallsweep.StallSweeper {
	t.Helper()
	sweeper, err := stallsweep.NewStallSweeper(stallsweep.StallConfig{
		Pool: config.Pool, InstanceID: config.InstanceID,
		Manager: config.Lease, Handle: config.LiveOwner(),
		Source: backend, Events: stallEvents(backend), Engine: engine, Terminalizer: backend,
		StalledTimeout: stallTimeout, MaxRunDuration: maxDuration, Report: firstReport(report),
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
	createEngineRunHandle(t, backend, runID, startedAt, lastAppendAt, nil)
}

// writerClock is a test-controlled run-writer clock: the backend stamps every
// event with it (the event's own Time is ignored), so a run's history can be
// laid out in the past.
type writerClock struct{ at time.Time }

func (c *writerClock) now() time.Time { return c.at }

func stallEvents(backend *journalpg.Backend) func(context.Context, string) ([]journal.EventRecord, error) {
	return func(ctx context.Context, runID string) ([]journal.EventRecord, error) {
		return backend.ReadEvents(ctx, journal.RunRef{ID: runID, ReadOnly: true}, 0, 0)
	}
}

// setRecordedClock lays a fixture's history out on the journal's commit
// clock. Fixtures append every event now and stamp history through the writer
// clock, so by default recorded_at would read "now" for all of them. offset is
// how far the writer clock ran ahead of the database (zero for an honest
// writer); legacy NULLs the column, as for events stored before migration
// 0025, which sends the sweep to its writer-clock fallback.
func setRecordedClock(t *testing.T, config journalpg.Config, runID string, offset time.Duration, legacy bool) {
	t.Helper()
	statement := `UPDATE journal.run_event SET recorded_at=at-$3::interval WHERE instance_id=$1 AND run_id=$2`
	args := []any{config.InstanceID, runID, offset.String()}
	if legacy {
		statement = `UPDATE journal.run_event SET recorded_at=NULL WHERE instance_id=$1 AND run_id=$2`
		args = args[:2]
	}
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func createEngineRunHandle(
	t *testing.T,
	backend *journalpg.Backend,
	runID string,
	startedAt time.Time,
	lastAppendAt time.Time,
	clock *writerClock,
) (journal.RunHandle, journal.OwnerToken) {
	t.Helper()
	var options journal.RunWriterOptions
	if clock != nil {
		clock.at = startedAt
		options.Clock = clock.now
	}
	handle, owner, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{
		Identity: journal.RunIdentity{
			RunID: runID, Gaggle: "g", Workflow: "implementation",
			Driver: journal.DriverEngine, StartedAt: startedAt,
		},
		Options: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{
		Type: journal.EventRunnerAnnotation, Time: lastAppendAt,
	}}); err != nil {
		t.Fatal(err)
	}
	return handle, owner
}

func databaseClock(t *testing.T, config journalpg.Config) time.Time {
	t.Helper()
	var now time.Time
	if err := config.Pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC()
}

// ageRun backdates a run on the database clock, as the other stall tests do.
func ageRun(t *testing.T, config journalpg.Config, runID string, age, idle time.Duration) {
	t.Helper()
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run SET created_at=clock_timestamp()-$3::interval
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID, age.String(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := postgresOwnerPool(t, config).Exec(t.Context(), `
		UPDATE journal.run_seq SET last_append_at=clock_timestamp()-$3::interval
		WHERE instance_id=$1 AND run_id=$2`,
		config.InstanceID, runID, idle.String(),
	); err != nil {
		t.Fatal(err)
	}
}

// appendChildWait commits a stage start bound to its origin and a durable child
// wait that runs from waitFrom, optionally closed at waitTo (zero = still parked).
func appendChildWait(
	t *testing.T,
	backend *journalpg.Backend,
	handle journal.RunHandle,
	owner journal.OwnerToken,
	clock *writerClock,
	runID string,
	waitFrom, waitTo time.Time,
) {
	t.Helper()
	clock.at = waitFrom
	_, origin, err := backend.AppendChildStageStarted(t.Context(), handle, owner, journal.ChildStageStartRecord{
		Event: journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := "00000000000000000000000000099999"
	request := journal.ChildHandoffRequest{
		Gaggle: "g", ParentRunID: runID, RequestID: journal.Digest([]byte(child)), Action: "wait",
		ChildRunID: child, AcceptanceID: "trigger-" + child, InvocationKey: child,
		SourceDigest: journal.Digest([]byte("source")), Origin: *origin,
	}
	events := []journal.Event{{
		Type: journal.EventRunnerAnnotation, Stage: "build", Attempt: 1,
		Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": journal.ChildWaitHeader{
			Version: 1, ParentRunID: runID, Request: request}},
	}}
	if _, err := backend.AppendBatch(t.Context(), handle, owner, events); err != nil {
		t.Fatal(err)
	}
	if !waitTo.IsZero() {
		clock.at = waitTo
		if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{
			Type: journal.EventRunnerAnnotation, Stage: "build", Attempt: 1,
			Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": request.RequestID},
		}}); err != nil {
			t.Fatal(err)
		}
	}
}

func requireEvents(t *testing.T, backend *journalpg.Backend, runID string, check func([]journal.Event) bool, what string) {
	t.Helper()
	records, err := stallEvents(backend)(t.Context(), runID)
	events := make([]journal.Event, len(records))
	for i, record := range records {
		events[i] = record.Event
	}
	if err != nil || !check(events) {
		t.Fatalf("test precondition: %s (events=%d err=%v)", what, len(events), err)
	}
}

// The local sweep's one exemption: a run waiting at a human gate is not stalled.
func TestCloudStallSkipsRunParkedAtGate(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000057816"
	clock := &writerClock{}
	handle, owner := createEngineRunHandle(t, backend, runID, now.Add(-3*time.Hour), now.Add(-2*time.Hour), clock)
	clock.at = now.Add(-2 * time.Hour)
	if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{{
		Type: journal.EventGatePaused, Stage: "review",
	}}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, backend, runID, journal.ParkedAtGate, "run is parked at a gate")
	ageRun(t, config, runID, 3*time.Hour, 2*time.Hour)
	engine := &stallEngine{}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	if result, err := sweeper.Sweep(t.Context()); err != nil || result.Cancelled != 0 || len(engine.cancelled) != 0 {
		t.Fatalf("gate-parked run was swept: %+v %v cancelled=%v", result, err, engine.cancelled)
	}
	// maxRunDuration still applies at a gate (only child waits are excluded),
	// as in the local sweep.
	bounded := cloudSweeper(t, config, backend, engine, 30*time.Minute, time.Hour)
	if result, err := bounded.Sweep(t.Context()); err != nil || result.Cancelled != 1 {
		t.Fatalf("max duration at a gate = %+v %v", result, err)
	}
}

func TestCloudStallSkipsRunParkedOnChild(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000067816"
	clock := &writerClock{}
	handle, owner := createEngineRunHandle(t, backend, runID, now.Add(-3*time.Hour), now.Add(-3*time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, runID, now.Add(-2*time.Hour), time.Time{})
	requireEvents(t, backend, runID, journal.ParkedOnChild, "run is parked on a child")
	ageRun(t, config, runID, 3*time.Hour, 2*time.Hour)
	engine := &stallEngine{}
	sweeper := cloudSweeper(t, config, backend, engine, 30*time.Minute, 0)
	if result, err := sweeper.Sweep(t.Context()); err != nil || result.Cancelled != 0 || len(engine.cancelled) != 0 {
		t.Fatalf("child-parked run was swept: %+v %v cancelled=%v", result, err, engine.cancelled)
	}
}

// Design 10.3: maxRunDuration is run age less durable child waits.
func TestCloudMaxRunDurationExcludesChildWaits(t *testing.T) {
	for _, test := range []struct {
		name      string
		waitStart time.Duration // before now
		waitEnd   time.Duration // before now
		wantDue   bool
	}{
		// age 2h, 90m of waiting: 30m of execution < 1h.
		{"long child wait keeps the run inside its budget", 110 * time.Minute, 20 * time.Minute, false},
		// age 2h, 30m of waiting: 90m of execution > 1h.
		{"short child wait still exceeds the budget", 60 * time.Minute, 30 * time.Minute, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := postgresJournalConfig(t)
			backend := postgresBackend(t, config)
			now := databaseClock(t, config)
			runID := "00000000000000000000000000077816"
			clock := &writerClock{}
			handle, owner := createEngineRunHandle(t, backend, runID, now.Add(-2*time.Hour), now.Add(-2*time.Hour), clock)
			appendChildWait(t, backend, handle, owner, clock, runID, now.Add(-test.waitStart), now.Add(-test.waitEnd))
			ageRun(t, config, runID, 2*time.Hour, 10*time.Minute)
			setRecordedClock(t, config, runID, 0, false)
			requireEvents(t, backend, runID, func(e []journal.Event) bool { return !journal.ParkedOnChild(e) }, "the wait is closed")
			engine := &stallEngine{}
			sweeper := cloudSweeper(t, config, backend, engine, time.Hour, time.Hour)
			result, err := sweeper.Sweep(t.Context())
			if err != nil || (result.Cancelled == 1) != test.wantDue {
				t.Fatalf("sweep = %+v %v, want due=%v", result, err, test.wantDue)
			}
		})
	}
}

// A child-wait return stamped a year ahead by a skewed writer must not buy the
// run extra time: the wait gets no credit and the raw database-clock age
// (2h against a 1h budget) cancels it. This is the writer-clock fallback for
// history stored before the journal recorded its commit clock.
func TestCloudMaxRunDurationIgnoresFutureStampedChildWait(t *testing.T) {
	config := postgresJournalConfig(t)
	backend := postgresBackend(t, config)
	now := databaseClock(t, config)
	runID := "00000000000000000000000000097816"
	clock := &writerClock{}
	handle, owner := createEngineRunHandle(t, backend, runID, now.Add(-2*time.Hour), now.Add(-2*time.Hour), clock)
	appendChildWait(t, backend, handle, owner, clock, runID, now.Add(-110*time.Minute), now.Add(365*24*time.Hour))
	ageRun(t, config, runID, 2*time.Hour, 10*time.Minute)
	setRecordedClock(t, config, runID, 0, true)
	engine := &stallEngine{}
	var reported []error
	sweeper := cloudSweeper(t, config, backend, engine, time.Hour, time.Hour, func(err error) { reported = append(reported, err) })
	result, err := sweeper.Sweep(t.Context())
	if err != nil || result.Cancelled != 1 {
		t.Fatalf("future-stamped wait evaded the budget: %+v %v", result, err)
	}
	if len(reported) != 1 {
		t.Fatalf("reported = %v, want the skew finding", reported)
	}
}

func firstReport(reports []func(error)) func(error) {
	if len(reports) == 0 {
		return nil
	}
	return reports[0]
}

// TestCloudMaxRunDurationIsImmuneToWriterClockSkew (#8107): with the
// journal's commit clock recorded, a writer whose clock runs hours ahead or
// behind gets exactly the child-wait credit an honest writer gets, with no
// skew finding: the sweep never reads the writer's clock.
func TestCloudMaxRunDurationIsImmuneToWriterClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{6 * time.Hour, -6 * time.Hour} {
		for _, test := range []struct {
			name               string
			waitStart, waitEnd time.Duration // before now, on the database clock
			wantDue            bool
		}{
			{"long child wait keeps the run inside its budget", 110 * time.Minute, 20 * time.Minute, false},
			{"short child wait still exceeds the budget", 60 * time.Minute, 30 * time.Minute, true},
		} {
			t.Run(fmt.Sprintf("%s/%s", skew, test.name), func(t *testing.T) {
				config := postgresJournalConfig(t)
				backend := postgresBackend(t, config)
				now := databaseClock(t, config)
				runID := "00000000000000000000000000087816"
				clock := &writerClock{}
				handle, owner := createEngineRunHandle(t, backend, runID, now.Add(-2*time.Hour+skew), now.Add(-2*time.Hour+skew), clock)
				appendChildWait(t, backend, handle, owner, clock, runID, now.Add(-test.waitStart+skew), now.Add(-test.waitEnd+skew))
				ageRun(t, config, runID, 2*time.Hour, 10*time.Minute)
				setRecordedClock(t, config, runID, skew, false)
				engine := &stallEngine{}
				var reported []error
				sweeper := cloudSweeper(t, config, backend, engine, time.Hour, time.Hour, func(err error) { reported = append(reported, err) })
				result, err := sweeper.Sweep(t.Context())
				if err != nil || (result.Cancelled == 1) != test.wantDue || len(reported) != 0 {
					t.Fatalf("sweep = %+v %v reported=%v, want due=%v with no skew finding", result, err, reported, test.wantDue)
				}
			})
		}
	}
}
