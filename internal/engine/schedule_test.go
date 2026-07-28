package engine

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type fakeScheduleClient struct {
	schedules map[string]*client.ScheduleDescription
	creates   int
	updates   int
	deletes   int
}

func newFakeScheduleClient() *fakeScheduleClient {
	return &fakeScheduleClient{schedules: make(map[string]*client.ScheduleDescription)}
}

func (f *fakeScheduleClient) Create(_ context.Context, options client.ScheduleOptions) (client.ScheduleHandle, error) {
	if _, exists := f.schedules[options.ID]; exists {
		return nil, sdktemporal.ErrScheduleAlreadyRunning
	}
	f.creates++
	f.schedules[options.ID] = &client.ScheduleDescription{
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
	return fakeScheduleHandle{client: f, id: options.ID}, nil
}

func (f *fakeScheduleClient) List(context.Context, client.ScheduleListOptions) (client.ScheduleListIterator, error) {
	ids := make([]string, 0, len(f.schedules))
	for id := range f.schedules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &fakeScheduleIterator{ids: ids}, nil
}

func (f *fakeScheduleClient) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return fakeScheduleHandle{client: f, id: id}
}

func (f *fakeScheduleClient) fire(id string) bool {
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

type fakeScheduleIterator struct {
	ids []string
	at  int
}

func (f *fakeScheduleIterator) HasNext() bool { return f.at < len(f.ids) }

func (f *fakeScheduleIterator) Next() (*client.ScheduleListEntry, error) {
	if !f.HasNext() {
		return nil, errors.New("iterator exhausted")
	}
	entry := &client.ScheduleListEntry{ID: f.ids[f.at]}
	f.at++
	return entry, nil
}

type fakeScheduleHandle struct {
	client *fakeScheduleClient
	id     string
}

func (f fakeScheduleHandle) GetID() string { return f.id }

func (f fakeScheduleHandle) Delete(context.Context) error {
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
	f.client.schedules[f.id].Schedule.State.Paused = true
	return nil
}

func (f fakeScheduleHandle) Unpause(context.Context, client.ScheduleUnpauseOptions) error {
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
		InstanceID: "prod-west",
		ConfigSHA:  "abc123",
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
	reconciler, err := NewScheduleReconciler(store, "goobers-engine", catchup)
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
	if err := reconciler.Reconcile(context.Background(), updated); err != nil {
		t.Fatalf("update reconcile: %v", err)
	}
	if store.updates != 1 || store.schedules[id].Schedule.Spec.CronExpressions[0] != "30 * * * *" {
		t.Fatalf("schedule was not updated in place: updates=%d", store.updates)
	}

	removed := updated
	removed.ConfigSHA = "fed987"
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
	if _, err := NewScheduleReconciler(store, "goobers-engine", 9*time.Second); err == nil {
		t.Fatal("catch-up below Temporal's minimum was accepted")
	}
	if _, err := NewScheduleReconciler(store, "goobers-engine", 25*time.Hour); err == nil {
		t.Fatal("unbounded catch-up window was accepted")
	}
}

func TestScheduleOverlapSkipsWhileRunInFlight(t *testing.T) {
	store := newFakeScheduleClient()
	reconciler, err := NewScheduleReconciler(store, "goobers-engine", time.Minute)
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
