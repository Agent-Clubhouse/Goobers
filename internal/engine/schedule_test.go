package engine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

type fakeScheduleClient struct {
	mu               sync.Mutex
	schedules        map[string]*client.ScheduleDescription
	states           map[string]fakeScheduleState
	creates          int
	updates          int
	deletes          int
	applyDelay       time.Duration
	activeApplies    int
	maxActiveApplies int
}

type fakeScheduleState struct {
	state   scheduleReconcileState
	version uint64
}

func newFakeScheduleClient() *fakeScheduleClient {
	return &fakeScheduleClient{
		schedules: make(map[string]*client.ScheduleDescription),
		states:    make(map[string]fakeScheduleState),
	}
}

func (f *fakeScheduleClient) Create(_ context.Context, options client.ScheduleOptions) (client.ScheduleHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.schedules[options.ID]; exists {
		return nil, sdktemporal.ErrScheduleAlreadyRunning
	}
	f.creates++
	f.schedules[options.ID] = fakeScheduleDescription(options)
	return fakeScheduleHandle{client: f, id: options.ID}, nil
}

func fakeScheduleDescription(options client.ScheduleOptions) *client.ScheduleDescription {
	return &client.ScheduleDescription{
		Schedule: client.Schedule{
			Action: options.Action,
			Spec:   &options.Spec,
			Policy: &client.SchedulePolicies{
				Overlap:        options.Overlap,
				CatchupWindow:  options.CatchupWindow,
				PauseOnFailure: options.PauseOnFailure,
			},
			State: &client.ScheduleState{Note: options.Note},
		},
	}
}

func (f *fakeScheduleClient) ApplySchedule(
	ctx context.Context,
	options client.ScheduleOptions,
	_ bool,
) error {
	f.mu.Lock()
	f.activeApplies++
	if f.activeApplies > f.maxActiveApplies {
		f.maxActiveApplies = f.activeApplies
	}
	delay := f.applyDelay
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.activeApplies--
		f.mu.Unlock()
	}()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	handle := f.GetHandle(ctx, options.ID)
	description, err := handle.Describe(ctx)
	if isScheduleNotFound(err) {
		_, err = f.Create(ctx, options)
		if errors.Is(err, sdktemporal.ErrScheduleAlreadyRunning) {
			return f.ApplySchedule(ctx, options, true)
		}
		return err
	}
	if err != nil {
		return err
	}
	currentGeneration, currentManaged := managedScheduleGeneration(description.Schedule.State.Note)
	desiredGeneration, desiredManaged := managedScheduleGeneration(options.Note)
	if currentManaged && desiredManaged && currentGeneration > desiredGeneration {
		return nil
	}
	if description.Schedule.State != nil && description.Schedule.State.Note == options.Note {
		return nil
	}
	return handle.Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(input client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			if input.Description.Schedule.State != nil &&
				input.Description.Schedule.State.Note == options.Note {
				return nil, sdktemporal.ErrSkipScheduleUpdate
			}
			schedule := fakeScheduleDescription(options).Schedule
			return &client.ScheduleUpdate{Schedule: &schedule}, nil
		},
	})
}

func (f *fakeScheduleClient) DeleteSchedule(ctx context.Context, id string, generation uint64) error {
	description, err := f.GetHandle(ctx, id).Describe(ctx)
	if err != nil {
		return err
	}
	if currentGeneration, managed := managedScheduleGeneration(description.Schedule.State.Note); managed && currentGeneration > generation {
		return nil
	}
	return f.GetHandle(ctx, id).Delete(ctx)
}

func (f *fakeScheduleClient) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return fakeScheduleHandle{client: f, id: id}
}

func (f *fakeScheduleClient) fire(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	description := f.schedules[id]
	if description.Schedule.Policy.Overlap == enumspb.SCHEDULE_OVERLAP_POLICY_SKIP &&
		len(description.Info.RunningWorkflows) > 0 {
		description.Info.NumActionsSkippedOverlap++
		return false
	}
	description.Info.RunningWorkflows = append(description.Info.RunningWorkflows, client.ScheduleWorkflowExecution{
		WorkflowID: id + "-active",
	})
	return true
}

func (f *fakeScheduleClient) CreateState(
	_ context.Context,
	options client.ScheduleOptions,
	state scheduleReconcileState,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.states[options.ID]; exists {
		return sdktemporal.ErrScheduleAlreadyRunning
	}
	f.states[options.ID] = fakeScheduleState{state: state, version: 1}
	return nil
}

func (f *fakeScheduleClient) LoadState(_ context.Context, id string) (scheduleStateRevision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.states[id]
	if !exists {
		return scheduleStateRevision{}, serviceerror.NewNotFound("schedule state not found")
	}
	return scheduleStateRevision{state: record.state, fakeVersion: record.version}, nil
}

func (f *fakeScheduleClient) CompareAndSwapState(
	_ context.Context,
	id string,
	revision scheduleStateRevision,
	state scheduleReconcileState,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.states[id]
	if !exists {
		return serviceerror.NewNotFound("schedule state not found")
	}
	if record.version != revision.fakeVersion {
		return errScheduleStateConflict
	}
	record.state = state
	record.version++
	f.states[id] = record
	return nil
}

type fakeScheduleHandle struct {
	client *fakeScheduleClient
	id     string
}

func (f fakeScheduleHandle) GetID() string { return f.id }

func (f fakeScheduleHandle) Delete(context.Context) error {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	if _, exists := f.client.schedules[f.id]; !exists {
		return serviceerror.NewNotFound("schedule not found")
	}
	delete(f.client.schedules, f.id)
	f.client.deletes++
	return nil
}

func (f fakeScheduleHandle) Backfill(context.Context, client.ScheduleBackfillOptions) error {
	return nil
}

func (f fakeScheduleHandle) Update(_ context.Context, options client.ScheduleUpdateOptions) error {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	current, exists := f.client.schedules[f.id]
	if !exists {
		return serviceerror.NewNotFound("schedule not found")
	}
	update, err := options.DoUpdate(client.ScheduleUpdateInput{Description: *current})
	if errors.Is(err, sdktemporal.ErrSkipScheduleUpdate) {
		return nil
	}
	if err != nil {
		return err
	}
	f.client.updates++
	current.Schedule = *update.Schedule
	return nil
}

func (f fakeScheduleHandle) Describe(context.Context) (*client.ScheduleDescription, error) {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	description, exists := f.client.schedules[f.id]
	if !exists {
		return nil, serviceerror.NewNotFound("schedule not found")
	}
	copy := *description
	return &copy, nil
}

func (f fakeScheduleHandle) Trigger(context.Context, client.ScheduleTriggerOptions) error {
	f.client.fire(f.id)
	return nil
}

func (f fakeScheduleHandle) Pause(context.Context, client.SchedulePauseOptions) error {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.schedules[f.id].Schedule.State.Paused = true
	return nil
}

func (f fakeScheduleHandle) Unpause(context.Context, client.ScheduleUnpauseOptions) error {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.schedules[f.id].Schedule.State.Paused = false
	return nil
}

func scheduledSnapshot(cron string) ScheduleSnapshot {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	spec.Triggers = []apiv1.Trigger{
		{Type: apiv1.TriggerSchedule, Schedule: cron},
		{Type: apiv1.TriggerSignal, Signal: "manual-nudge"},
	}
	preview := true
	return ScheduleSnapshot{
		InstanceID:       "prod-west",
		ConfigSHA:        "abc123",
		ConfigGeneration: 1,
		Runs: []RunInput{{
			Gaggle:                 "web",
			WorkflowName:           "implement",
			Version:                3,
			PreviewFeaturesEnabled: &preview,
			Spec:                   spec,
		}},
	}
}

func TestScheduleReconcilerLifecycleAndPolicies(t *testing.T) {
	const catchup = 15 * time.Minute
	store := newFakeScheduleClient()
	reconciler, err := newScheduleReconciler(store, "goobers-engine", catchup)
	if err != nil {
		t.Fatalf("NewScheduleReconciler: %v", err)
	}
	snapshot := scheduledSnapshot("0 * * * *")

	if err := reconciler.Reconcile(context.Background(), snapshot); err != nil {
		t.Fatalf("create reconcile: %v", err)
	}
	id := ScheduleID("prod-west", "web", "implement", 0)
	if store.creates != 1 || len(store.schedules) != 1 {
		t.Fatalf("creates=%d schedules=%d, want one", store.creates, len(store.schedules))
	}
	description := store.schedules[id]
	if description == nil {
		t.Fatalf("deterministic schedule %q was not created", id)
	}
	if got := description.Schedule.Spec.CronExpressions; len(got) != 1 || got[0] != "0 * * * *" {
		t.Fatalf("cron expressions = %v", got)
	}
	if description.Schedule.Policy.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP {
		t.Fatalf("overlap = %v, want SKIP", description.Schedule.Policy.Overlap)
	}
	if description.Schedule.Policy.CatchupWindow != catchup {
		t.Fatalf("catch-up = %v, want %v", description.Schedule.Policy.CatchupWindow, catchup)
	}
	action, ok := description.Schedule.Action.(*client.ScheduleWorkflowAction)
	if !ok {
		t.Fatalf("action type = %T, want ScheduleWorkflowAction", description.Schedule.Action)
	}
	if action.ID != id || action.TaskQueue != "goobers-engine" {
		t.Fatalf("action identity = %q queue = %q", action.ID, action.TaskQueue)
	}
	if action.RetryPolicy == nil || action.RetryPolicy.MaximumAttempts != 1 {
		t.Fatalf("retry policy = %+v, want explicit single attempt", action.RetryPolicy)
	}
	if len(action.Args) != 1 {
		t.Fatalf("action args = %d, want pinned RunInput", len(action.Args))
	}
	run, ok := action.Args[0].(RunInput)
	if !ok || run.RunID != "" || run.TriggerKind != string(journal.TriggerSchedule) || run.TriggerRef != id {
		t.Fatalf("scheduled run input = %#v", action.Args[0])
	}

	if err := reconciler.Reconcile(context.Background(), snapshot); err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	if store.creates != 1 || store.updates != 0 || store.deletes != 0 {
		t.Fatalf("second reconcile mutated schedules: creates=%d updates=%d deletes=%d", store.creates, store.updates, store.deletes)
	}

	updated := scheduledSnapshot("30 * * * *")
	updated.ConfigSHA = "def456"
	updated.ConfigGeneration = 2
	if err := reconciler.Reconcile(context.Background(), updated); err != nil {
		t.Fatalf("update reconcile: %v", err)
	}
	if store.updates != 1 || store.schedules[id].Schedule.Spec.CronExpressions[0] != "30 * * * *" {
		t.Fatalf("schedule was not updated in place: updates=%d", store.updates)
	}

	removed := updated
	removed.ConfigSHA = "fed987"
	removed.ConfigGeneration = 3
	removed.Runs[0].Spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSignal, Signal: "manual-nudge"}}
	if err := reconciler.Reconcile(context.Background(), removed); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}
	if store.deletes != 1 || len(store.schedules) != 0 {
		t.Fatalf("removed trigger was not deleted: deletes=%d schedules=%d", store.deletes, len(store.schedules))
	}
}

func TestScheduleReconcilerRequiresBoundedCatchup(t *testing.T) {
	store := newFakeScheduleClient()
	if _, err := newScheduleReconciler(store, "goobers-engine", 9*time.Second); err == nil {
		t.Fatal("catch-up below Temporal's minimum was accepted")
	}
	if _, err := newScheduleReconciler(store, "goobers-engine", 25*time.Hour); err == nil {
		t.Fatal("unbounded catch-up window was accepted")
	}
}

func TestScheduleOverlapSkipsWhileRunInFlight(t *testing.T) {
	store := newFakeScheduleClient()
	reconciler, err := newScheduleReconciler(store, "goobers-engine", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(context.Background(), scheduledSnapshot("@every 1m")); err != nil {
		t.Fatal(err)
	}
	id := ScheduleID("prod-west", "web", "implement", 0)
	if !store.fire(id) {
		t.Fatal("first fire should start")
	}
	if store.fire(id) {
		t.Fatal("overlapping fire should be skipped")
	}
	if got := store.schedules[id].Info.NumActionsSkippedOverlap; got != 1 {
		t.Fatalf("skipped overlap actions = %d, want 1", got)
	}
}

func TestScheduleReconcilerStaleOwnerCannotResurrectRemovedSchedule(t *testing.T) {
	store := newFakeScheduleClient()
	reconciler, err := newScheduleReconciler(store, "goobers-engine", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	initial := scheduledSnapshot("0 * * * *")
	if err := reconciler.Reconcile(context.Background(), initial); err != nil {
		t.Fatal(err)
	}

	stateID := scheduleOwnedPrefix(initial.InstanceID) + "state"
	revision, err := store.LoadState(context.Background(), stateID)
	if err != nil {
		t.Fatal(err)
	}
	staleSnapshot := scheduledSnapshot("15 * * * *")
	staleSnapshot.ConfigSHA = "stale"
	staleSnapshot.ConfigGeneration = 4
	stale := revision.state
	stale.AppliedGeneration = 3
	stale.AppliedConfigSHA = "newer"
	stale.ManagedScheduleIDs = nil
	stale.Pending = &pendingScheduleReconcile{
		Snapshot: staleSnapshot,
		Owner:    "stale-owner",
	}
	if err := store.CompareAndSwapState(context.Background(), stateID, revision, stale); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	delete(store.schedules, ScheduleID(initial.InstanceID, "web", "implement", 0))
	store.mu.Unlock()

	newer := scheduledSnapshot("30 * * * *")
	newer.ConfigSHA = "newest"
	newer.ConfigGeneration = 5
	if err := reconciler.Reconcile(context.Background(), newer); !errors.Is(err, errScheduleStateConflict) {
		t.Fatalf("competing durable reconcile error = %v, want conflict", err)
	}
	if _, exists := store.schedules[ScheduleID(initial.InstanceID, "web", "implement", 0)]; exists {
		t.Fatal("stale owner resurrected a removed schedule")
	}
}

type blockingScheduleStages struct {
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	calls       int
}

func newBlockingScheduleStages() *blockingScheduleStages {
	return &blockingScheduleStages{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingScheduleStages) Run(
	ctx context.Context,
	_ apiv1.InvocationEnvelope,
	_ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.startOnce.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	case <-ctx.Done():
		return apiv1.ResultEnvelope{}, ctx.Err()
	}
}

func (b *blockingScheduleStages) releaseRun() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingScheduleStages) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestTemporalScheduleLifecycleClaimsAndOverlap(t *testing.T) {
	startupCtx, cancelStartup := context.WithTimeout(t.Context(), 5*time.Minute)
	server, err := temporaltest.StartDevServer(startupCtx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		ExtraArgs: []string{
			"--dynamic-config-value", "history.enableCHASMSchedulerCreation=true",
		},
	})
	cancelStartup()
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	const (
		namespace = "default"
		taskQueue = "goobers-schedule-integration"
	)
	temporalClient := server.Client()
	scheduleService := temporalClient.WorkflowService()
	blocker := newBlockingScheduleStages()
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	RegisterWith(temporalWorker, &Activities{
		Det:             blocker,
		Workspaces:      testWorkspaces(t),
		ScheduleService: scheduleService,
	})
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal worker: %v", err)
	}
	t.Cleanup(temporalWorker.Stop)
	t.Cleanup(blocker.releaseRun)

	const catchup = 15 * time.Minute
	reconciler, err := NewScheduleReconciler(temporalClient, namespace, taskQueue, catchup)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := scheduledSnapshot("0 0 1 1 *")
	if err := reconciler.Reconcile(ctx, snapshot); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	id := ScheduleID("prod-west", "web", "implement", 0)
	handle := temporalClient.ScheduleClient().GetHandle(ctx, id)
	created, err := handle.Describe(ctx)
	if err != nil {
		t.Fatalf("describe created schedule: %v", err)
	}
	assertTemporalSchedulePolicy(t, created, id, taskQueue, catchup)
	if err := reconciler.Reconcile(ctx, snapshot); err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	unchanged, err := handle.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.Info.LastUpdateAt.Equal(created.Info.LastUpdateAt) {
		t.Fatalf("idempotent reconcile changed schedule at %v", unchanged.Info.LastUpdateAt)
	}

	intermediate := scheduledSnapshot("0 0 3 1 *")
	intermediate.ConfigSHA = "def456"
	intermediate.ConfigGeneration = 2
	updated := scheduledSnapshot("0 0 2 1 *")
	updated.ConfigSHA = "ghi789"
	updated.ConfigGeneration = 3
	secondReconciler, err := NewScheduleReconciler(temporalClient, namespace, taskQueue, catchup)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- reconciler.Reconcile(ctx, intermediate)
	}()
	go func() {
		<-start
		errs <- secondReconciler.Reconcile(ctx, updated)
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent schedule update: %v", err)
		}
	}
	description, err := handle.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runInput := decodeTemporalScheduleInput(t, description)
	if got := runInput.Spec.Triggers[0].Schedule; got != "0 0 2 1 *" {
		t.Fatalf("updated schedule input cron = %q", got)
	}

	fireTime := time.Date(time.Now().UTC().Year()-1, time.January, 2, 0, 0, 0, 0, time.UTC)
	backfill := client.ScheduleBackfillOptions{Backfill: []client.ScheduleBackfill{{
		Start:   fireTime.Add(-time.Minute),
		End:     fireTime.Add(time.Minute),
		Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_ALLOW_ALL,
	}}}
	if err := handle.Backfill(ctx, backfill); err != nil {
		t.Fatalf("backfill schedule: %v", err)
	}
	select {
	case <-blocker.started:
	case <-ctx.Done():
		t.Fatalf("scheduled workflow did not reach its activity: %v", ctx.Err())
	}
	running := waitForSchedule(t, ctx, handle, func(description *client.ScheduleDescription) bool {
		return len(description.Info.RunningWorkflows) == 1
	})
	claimID := running.Info.RunningWorkflows[0].WorkflowID
	if want := ScheduleClaimID(id, fireTime); claimID != want {
		t.Fatalf("generated claim ID = %q, want %q", claimID, want)
	}

	if err := handle.Backfill(ctx, backfill); err != nil {
		t.Fatalf("duplicate backfill: %v", err)
	}
	if err := handle.Trigger(ctx, client.ScheduleTriggerOptions{}); err != nil {
		t.Fatalf("trigger overlapping fire: %v", err)
	}
	overlapped := waitForSchedule(t, ctx, handle, func(description *client.ScheduleDescription) bool {
		return description.Info.NumActionsSkippedOverlap >= 1
	})
	if len(overlapped.Info.RunningWorkflows) != 1 || blocker.callCount() != 1 {
		t.Fatalf("running workflows = %d, activity calls = %d, want exactly one", len(overlapped.Info.RunningWorkflows), blocker.callCount())
	}

	blocker.releaseRun()
	var result RunResult
	if err := temporalClient.GetWorkflow(ctx, claimID, "").Get(ctx, &result); err != nil {
		t.Fatalf("scheduled workflow result: %v", err)
	}
	runsDir := filepath.Join(t.TempDir(), "runs")
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	projectedDir, err := ProjectCompletedScheduledRun(ctx, temporalClient, claimID, runsDir, schedulerDir)
	if err != nil {
		t.Fatalf("project scheduled workflow: %v", err)
	}
	projected, err := journal.OpenRead(projectedDir)
	if err != nil {
		t.Fatalf("open projected scheduled workflow: %v", err)
	}
	identity, err := projected.Identity()
	if err != nil {
		t.Fatalf("read projected scheduled identity: %v", err)
	}
	if identity.RunID != RunID(claimID) || identity.Trigger.Kind != journal.TriggerSchedule {
		t.Fatalf("projected scheduled identity = %+v", identity)
	}
	schedulerEvents, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatalf("read projected scheduler events: %v", err)
	}
	if len(schedulerEvents) != 1 || schedulerEvents[0].Type != journal.EventTriggerFired ||
		schedulerEvents[0].RunID != RunID(claimID) {
		t.Fatalf("projected scheduler events = %+v", schedulerEvents)
	}
	duplicate, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       claimID,
		TaskQueue:                                taskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		RetryPolicy: &sdktemporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	}, ClaimScheduled, runInput)
	if err != nil {
		t.Fatalf("start duplicate completed schedule action: %v", err)
	}
	if err := duplicate.Get(ctx, &result); err != nil {
		t.Fatalf("duplicate completed schedule action: %v", err)
	}
	if got := blocker.callCount(); got != 1 {
		t.Fatalf("activity calls after completed duplicate = %d, want exactly one", got)
	}

	removed := updated
	removed.ConfigSHA = "fed987"
	removed.ConfigGeneration = 4
	removed.Runs[0].Spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSignal, Signal: "manual-nudge"}}
	if err := reconciler.Reconcile(ctx, removed); err != nil {
		t.Fatalf("remove schedule: %v", err)
	}
	if _, err := handle.Describe(ctx); !isScheduleNotFound(err) {
		t.Fatalf("describe removed schedule error = %v, want not found", err)
	}
}

func assertTemporalSchedulePolicy(
	t *testing.T,
	description *client.ScheduleDescription,
	id string,
	taskQueue string,
	catchup time.Duration,
) {
	t.Helper()
	if description.Schedule.Policy == nil ||
		description.Schedule.Policy.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP ||
		description.Schedule.Policy.CatchupWindow != catchup {
		t.Fatalf("schedule policy = %+v", description.Schedule.Policy)
	}
	action, ok := description.Schedule.Action.(*client.ScheduleWorkflowAction)
	if !ok {
		t.Fatalf("action type = %T", description.Schedule.Action)
	}
	if action.ID != id || action.TaskQueue != taskQueue ||
		action.RetryPolicy == nil || action.RetryPolicy.MaximumAttempts != 1 {
		t.Fatalf("schedule action = %+v", action)
	}
}

func decodeTemporalScheduleInput(t *testing.T, description *client.ScheduleDescription) RunInput {
	t.Helper()
	action, ok := description.Schedule.Action.(*client.ScheduleWorkflowAction)
	if !ok || len(action.Args) != 1 {
		t.Fatalf("schedule action = %#v", description.Schedule.Action)
	}
	payload, ok := action.Args[0].(*commonpb.Payload)
	if !ok {
		t.Fatalf("schedule argument type = %T", action.Args[0])
	}
	var input RunInput
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &input); err != nil {
		t.Fatalf("decode schedule input: %v", err)
	}
	return input
}

func waitForSchedule(
	t *testing.T,
	ctx context.Context,
	handle client.ScheduleHandle,
	ready func(*client.ScheduleDescription) bool,
) *client.ScheduleDescription {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		description, err := handle.Describe(ctx)
		if err == nil && ready(description) {
			return description
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Temporal schedule: %v (last describe error: %v)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

type rejectingDuplicateStarter struct {
	started map[string]int
}

func (r *rejectingDuplicateStarter) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	if r.started == nil {
		r.started = make(map[string]int)
	}
	if r.started[options.ID] > 0 {
		return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("duplicate claim", "request", "run")
	}
	r.started[options.ID]++
	return fakeRun{id: options.ID, runID: "run"}, nil
}

func TestScheduleClaimDuplicateStartsExactlyOneRun(t *testing.T) {
	scheduleID := ScheduleID("prod-west", "web", "implement", 0)
	fireTime := time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC)
	claimID := ScheduleClaimID(scheduleID, fireTime)
	if claimID != scheduleID+"-2026-07-28T04:00:00Z" {
		t.Fatalf("claim ID = %q", claimID)
	}

	backend := &rejectingDuplicateStarter{}
	starter := &TemporalStarter{client: backend, taskQueue: "goobers-engine"}
	in := sampleInput()
	in.RunID = claimID
	first, err := starter.Start(context.Background(), in)
	if err != nil || first.AlreadyRunning {
		t.Fatalf("first start = %+v, %v", first, err)
	}
	second, err := starter.Start(context.Background(), in)
	if err != nil || !second.AlreadyRunning {
		t.Fatalf("duplicate start = %+v, %v", second, err)
	}
	if backend.started[claimID] != 1 {
		t.Fatalf("started runs = %d, want exactly one", backend.started[claimID])
	}
}

func TestScheduledRunProjectsTriggerFire(t *testing.T) {
	scheduleID := ScheduleID("prod-west", "web", "implement", 0)
	fireTime := time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC)
	claimID := ScheduleClaimID(scheduleID, fireTime)
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}}
	in := projectionInput("implement", spec)
	in.RunID = ""
	in.TriggerRef = scheduleID

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: claimID})
	env.RegisterActivity(&Activities{Det: &scriptedStages{}, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(RunScheduled, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("RunScheduled: %v", err)
	}
	value, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query projection: %v", err)
	}
	var projection JournalProjection
	if err := value.Get(&projection); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	runID := RunID(claimID)
	if projection.Identity.RunID != runID || projection.Identity.Trigger.Ref != scheduleID {
		t.Fatalf("projected identity = %+v", projection.Identity)
	}
	if len(projection.SchedulerOps) != 1 {
		t.Fatalf("scheduler ops = %d, want one trigger fire", len(projection.SchedulerOps))
	}
	op := projection.SchedulerOps[0]
	if !op.Time.Equal(fireTime) || op.Event.Type != journal.EventTriggerFired ||
		op.Event.Workflow != "implement" || op.Event.Gaggle != "web" ||
		op.Event.RunID != runID || op.Event.Reason != "scheduled" {
		t.Fatalf("trigger projection = %+v", op)
	}

	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	if err := ProjectSchedulerEvents(schedulerDir, projection); err != nil {
		t.Fatalf("ProjectSchedulerEvents: %v", err)
	}
	if err := ProjectSchedulerEvents(schedulerDir, projection); err != nil {
		t.Fatalf("idempotent ProjectSchedulerEvents: %v", err)
	}
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	if len(events) != 1 || events[0].Type != journal.EventTriggerFired ||
		events[0].RunID != runID || !events[0].Time.Equal(fireTime) {
		t.Fatalf("projected scheduler events = %+v", events)
	}
}
