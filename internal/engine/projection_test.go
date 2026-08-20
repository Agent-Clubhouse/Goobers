package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

type completedRunFake struct {
	projection JournalProjection
	executions []*workflowpb.WorkflowExecutionInfo
	queries    int
}

type projectionValue struct {
	projection JournalProjection
}

func (v projectionValue) HasValue() bool { return true }

func (v projectionValue) Get(valuePtr interface{}) error {
	out, ok := valuePtr.(*JournalProjection)
	if !ok {
		return errors.New("unexpected projection destination")
	}
	*out = v.projection
	return nil
}

func (f *completedRunFake) ListWorkflow(_ context.Context, _ *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return &workflowservice.ListWorkflowExecutionsResponse{Executions: f.executions}, nil
}

func (f *completedRunFake) QueryWorkflow(_ context.Context, _, _, _ string, _ ...interface{}) (converter.EncodedValue, error) {
	f.queries++
	return projectionValue{projection: f.projection}, nil
}

// executeForProjection runs one engine fixture in the Temporal test
// environment and returns its queried journal projection. wantWorkflowErr
// tolerates a failed workflow — a failed run still projects (the projection is
// a function of history, not of success).
func executeForProjection(t *testing.T, in RunInput, acts *Activities, wantWorkflowErr bool) JournalProjection {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	// Pin the mock clock so two executions of the same fixture replay the
	// same deterministic timeline (the projection's op times come from
	// workflow.Now).
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); (err != nil) != wantWorkflowErr {
		t.Fatalf("workflow error = %v, wantWorkflowErr = %t", err, wantWorkflowErr)
	}
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	return proj
}

func projectionInput(name string, spec apiv1.WorkflowSpec) RunInput {
	in := runInput(name, spec)
	in.TriggerKind = string(journal.TriggerManual)
	return in
}

func TestCompletedRunReconcilerRetriesObservationForRecordedJournal(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("project-automatic", spec), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	payload, err := converter.GetDefaultDataConverter().ToPayload("web")
	if err != nil {
		t.Fatal(err)
	}
	fake := &completedRunFake{
		projection: proj,
		executions: []*workflowpb.WorkflowExecutionInfo{{
			Execution: &commonpb.WorkflowExecution{WorkflowId: proj.Identity.RunID},
			Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: payload}},
		}},
	}
	runsDir := filepath.Join(t.TempDir(), "runs")
	var (
		observeCalls int
		observedSeq  uint64
	)
	reconciler, err := NewCompletedRunReconciler(fake, "default", map[string]string{"web": runsDir}, func(_ context.Context, runID string, seq uint64) error {
		observeCalls++
		if runID != proj.Identity.RunID {
			t.Errorf("observed run id = %q, want %q", runID, proj.Identity.RunID)
		}
		observedSeq = seq
		if observeCalls == 1 {
			return errors.New("intake unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count, err := reconciler.Reconcile(context.Background()); err == nil || count != 0 {
		t.Fatalf("first Reconcile = (%d, %v), want (0, observer error)", count, err)
	}
	if observedSeq == 0 || !journal.Recorded(filepath.Join(runsDir, proj.Identity.RunID)) {
		t.Fatalf("projection was not published and observed: seq=%d", observedSeq)
	}
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 0 {
		t.Fatalf("second Reconcile = (%d, %v), want idempotent (0, nil)", count, err)
	}
	if fake.queries != 2 {
		t.Fatalf("projection queries = %d, want 2 (identity validation plus projection)", fake.queries)
	}
	if observeCalls != 2 {
		t.Fatalf("observer calls = %d, want 2", observeCalls)
	}
}

func TestCompletedRunReconcilerReplacesInterruptedProjection(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("project-interrupted", spec), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	payload, err := converter.GetDefaultDataConverter().ToPayload("web")
	if err != nil {
		t.Fatal(err)
	}
	fake := &completedRunFake{
		projection: proj,
		executions: []*workflowpb.WorkflowExecutionInfo{{
			Execution: &commonpb.WorkflowExecution{WorkflowId: proj.Identity.RunID},
			Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: payload}},
		}},
	}
	runsDir := filepath.Join(t.TempDir(), "runs")
	partial, err := journal.Create(runsDir, proj.Identity, map[string][]byte{
		journal.PinnedWorkflowGraphInputName: []byte(proj.Graph),
	})
	if err != nil {
		t.Fatalf("create interrupted projection: %v", err)
	}
	if err := partial.Close(); err != nil {
		t.Fatalf("close interrupted projection: %v", err)
	}

	var observedSeq uint64
	reconciler, err := NewCompletedRunReconciler(fake, "default", map[string]string{"web": runsDir}, func(_ context.Context, runID string, seq uint64) error {
		if runID != proj.Identity.RunID {
			t.Errorf("observed run id = %q, want %q", runID, proj.Identity.RunID)
		}
		observedSeq = seq
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 1 {
		t.Fatalf("Reconcile = (%d, %v), want (1, nil)", count, err)
	}
	if fake.queries != 2 {
		t.Fatalf("projection queries = %d, want 2", fake.queries)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, proj.Identity.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(proj.Ops) || events[len(events)-1].Type != journal.EventRunFinished {
		t.Fatalf("replacement events = %d ending in %s, want %d ending in run.finished", len(events), events[len(events)-1].Type, len(proj.Ops))
	}
	if observedSeq != events[len(events)-1].Seq {
		t.Fatalf("observed seq = %d, want terminal seq %d", observedSeq, events[len(events)-1].Seq)
	}
}

func TestProjectCompletedRunForGaggleObservesRecordedJournal(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("project-manual-existing", spec), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	runsDir := filepath.Join(t.TempDir(), "runs")
	dir, err := ProjectRun(runsDir, proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	fake := &completedRunFake{projection: proj}
	var observedSeq uint64
	gotDir, err := ProjectCompletedRunForGaggle(context.Background(), fake, proj.Identity.RunID, "web", runsDir, func(_ context.Context, runID string, seq uint64) error {
		if runID != proj.Identity.RunID {
			t.Errorf("observed run id = %q, want %q", runID, proj.Identity.RunID)
		}
		observedSeq = seq
		return nil
	})
	if err != nil {
		t.Fatalf("ProjectCompletedRunForGaggle: %v", err)
	}
	if gotDir != dir {
		t.Fatalf("projected dir = %q, want %q", gotDir, dir)
	}
	if observedSeq == 0 {
		t.Fatal("recorded journal was not observed")
	}
	if fake.queries != 0 {
		t.Fatalf("projection queries = %d, want 0 for recorded journal", fake.queries)
	}
}

func TestCompletedRunRecordedJournalFastPathsRejectTraversal(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("escaped-run", spec), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	payload, err := converter.GetDefaultDataConverter().ToPayload("web")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		run  func(context.Context, *completedRunFake, string, ProjectionObserver) error
	}{
		{
			name: "reconciler",
			run: func(ctx context.Context, fake *completedRunFake, runsDir string, observe ProjectionObserver) error {
				fake.executions = []*workflowpb.WorkflowExecutionInfo{{
					Execution: &commonpb.WorkflowExecution{WorkflowId: filepath.Join("..", proj.Identity.RunID)},
					Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: payload}},
				}}
				reconciler, err := NewCompletedRunReconciler(fake, "default", map[string]string{"web": runsDir}, observe)
				if err != nil {
					return err
				}
				count, err := reconciler.Reconcile(ctx)
				if count != 0 {
					t.Errorf("Reconcile count = %d, want 0", count)
				}
				return err
			},
		},
		{
			name: "manual",
			run: func(ctx context.Context, fake *completedRunFake, runsDir string, observe ProjectionObserver) error {
				_, err := ProjectCompletedRunForGaggle(
					ctx, fake, filepath.Join("..", proj.Identity.RunID), "web", runsDir, observe,
				)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runsDir := filepath.Join(root, "runs")
			if _, err := ProjectRun(root, proj); err != nil {
				t.Fatalf("create escaped recorded journal: %v", err)
			}
			fake := &completedRunFake{projection: proj}
			observeCalls := 0
			err := tc.run(context.Background(), fake, runsDir, func(context.Context, string, uint64) error {
				observeCalls++
				return nil
			})
			if !errors.Is(err, ErrUnprojectable) {
				t.Fatalf("err = %v, want ErrUnprojectable", err)
			}
			if observeCalls != 0 {
				t.Fatalf("observer calls = %d, want 0", observeCalls)
			}
			if fake.queries != 0 {
				t.Fatalf("projection queries = %d, want 0", fake.queries)
			}
		})
	}
}

func TestProjectionJournalsTypedIntegrityRefusal(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
			MinimumIntegrity: apiv1.IntegrityMaintainer,
			Run: &apiv1.DeterministicRun{
				Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch,
			},
		}},
	}
	in := projectionInput("integrity-refusal", spec)
	in.Item = &apiv1.BacklogItem{
		ID: "42", Provider: apiv1.ProviderGitHub, Integrity: apiv1.IntegrityUnapproved,
	}
	projection := executeForProjection(t, in, &Activities{}, true)

	var refusal *journal.Event
	for _, op := range projection.Ops {
		if op.Event == nil {
			continue
		}
		if op.Event.Type == journal.EventStageStarted {
			t.Fatalf("stage dispatched despite integrity refusal: %+v", op.Event)
		}
		if op.Event.Error != nil && op.Event.Error.Code == apiv1.IntegrityAdmissionErrorCode {
			refusal = op.Event
		}
	}
	if refusal == nil || refusal.Integrity != apiv1.IntegrityUnapproved ||
		refusal.MinimumIntegrity != apiv1.IntegrityMaintainer {
		t.Fatalf("typed refusal = %+v", refusal)
	}
}

func TestProjectionDefaultsMissingItemIntegrityConservatively(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
			MinimumIntegrity: apiv1.IntegrityUnapproved,
			Run:              &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		}},
	}
	in := projectionInput("integrity-normalization", spec)
	in.Item = &apiv1.BacklogItem{
		ID: "42", Provider: apiv1.ProviderGitHub, Labels: []string{"goobers:approved"},
	}
	var invocationIntegrity apiv1.Integrity
	projection := executeForProjection(t, in, &Activities{
		Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			invocationIntegrity = env.Item.Integrity
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}},
		Workspaces: testWorkspaces(t),
	}, false)
	if invocationIntegrity != apiv1.IntegrityUnapproved {
		t.Fatalf("invocation item integrity = %q, want unapproved", invocationIntegrity)
	}
	if projection.Item == nil || projection.Item.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("projected item = %+v, want unapproved integrity", projection.Item)
	}

	legacyProjection := projection
	legacyItem := *projection.Item
	legacyItem.Integrity = ""
	legacyProjection.Item = &legacyItem
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), legacyProjection)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	identity, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	var itemRef *journal.InputRef
	for i := range identity.Inputs {
		if identity.Inputs[i].Name == "item" {
			itemRef = &identity.Inputs[i]
			break
		}
	}
	if itemRef == nil || itemRef.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("item input ref = %+v, want unapproved integrity", itemRef)
	}
	data, err := os.ReadFile(filepath.Join(dir, itemRef.Ref.Path))
	if err != nil {
		t.Fatalf("read item snapshot: %v", err)
	}
	var snapshot apiv1.BacklogItem
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("decode item snapshot: %v", err)
	}
	if snapshot.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("item snapshot integrity = %q, want unapproved", snapshot.Integrity)
	}
}

// readDirBytes reads every regular file under dir into a path→content map, so
// two projected journals can be compared byte-for-byte.
func readDirBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestProjectRunDeterministic is #629's determinism criterion: projecting the
// same history twice yields byte-identical journals — event times, run.yaml,
// state.json, and artifact blobs all come from the deterministic workflow
// clock, never the projector's wall clock.
func TestProjectRunDeterministic(t *testing.T) {
	spec := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	proj := executeForProjection(t, projectionInput("proj-det", spec), &Activities{
		Det:        &scriptedStages{},
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
	}, false)

	dirA, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("first projection: %v", err)
	}
	dirB, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("second projection: %v", err)
	}

	a, b := readDirBytes(t, dirA), readDirBytes(t, dirB)
	if len(a) != len(b) {
		t.Fatalf("projected file sets differ: %d vs %d files", len(a), len(b))
	}
	for rel, dataA := range a {
		dataB, ok := b[rel]
		if !ok {
			t.Fatalf("second projection is missing %s", rel)
		}
		if string(dataA) != string(dataB) {
			t.Errorf("projected %s differs between runs:\nA: %s\nB: %s", rel, dataA, dataB)
		}
	}

	// The projected journal reads back through the standard reader with the
	// journal's own structural invariant intact.
	rd, err := journal.OpenRead(dirA)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("projected journal violates seq monotonicity: %v", err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.WorkflowDigest == "" || id.Workflow != "proj-det" || id.Trigger.Kind != journal.TriggerManual {
		t.Errorf("projected identity incomplete: %+v", id)
	}
	if len(id.Inputs) == 0 {
		t.Errorf("projected run.yaml pins no input snapshots (want the workflow graph)")
	}
}

// TestProjectRunFailedRunStillProjects covers the failed-workflow arm: a
// dispatch-exhausted run's history projects a journal ending run_failed +
// run.finished(failed), exactly like the local runner's failTerminal.
func TestProjectRunFailedRunStillProjects(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	proj := executeForProjection(t, projectionInput("proj-fail", spec), &Activities{
		Det:        &scriptedErrors{err: errors.New("tool exploded")},
		Workspaces: testWorkspaces(t),
	}, true)

	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseFailed) {
		t.Fatalf("last event = %+v, want run.finished failed", last)
	}
	cause := events[len(events)-2]
	if cause.Type != journal.EventError || cause.Error == nil || cause.Error.Code != "run_failed" {
		t.Fatalf("penultimate event = %+v, want the run_failed cause", cause)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state phase = %q, want failed", st.Phase)
	}
}

// scriptedErrors is an invoke.Deterministic that always fails dispatch.
type scriptedErrors struct{ err error }

func (s *scriptedErrors) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, s.err
}

// TestProjectRunFailsClosed pins the #629 fail-closed contract: history that
// cannot be projected to a normative journal is an error naming the offending
// op, never a silently skipped event.
func TestProjectRunFailsClosed(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	base := executeForProjection(t, projectionInput("proj-closed", spec), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)

	mutate := func(f func(p *JournalProjection)) JournalProjection {
		clone := base
		clone.Ops = append([]JournalOp(nil), base.Ops...)
		f(&clone)
		return clone
	}

	cases := []struct {
		name string
		proj JournalProjection
		want string
	}{
		{
			name: "unknown op kind",
			proj: mutate(func(p *JournalProjection) {
				p.Ops[1].Kind = "mystery"
			}),
			want: "unknown kind",
		},
		{
			name: "unknown event type",
			proj: mutate(func(p *JournalProjection) {
				ev := *p.Ops[1].Event
				ev.Type = "stage.morphed"
				p.Ops[1].Event = &ev
			}),
			want: "unknown event type",
		},
		{
			name: "unknown attempt class",
			proj: mutate(func(p *JournalProjection) {
				ev := *p.Ops[1].Event
				ev.AttemptClass = "cosmic"
				p.Ops[1].Event = &ev
			}),
			want: "unknown attempt class",
		},
		{
			name: "no terminal event",
			proj: mutate(func(p *JournalProjection) {
				p.Ops = p.Ops[:len(p.Ops)-1]
			}),
			want: "no terminal run.finished",
		},
		{
			name: "unknown terminal status",
			proj: mutate(func(p *JournalProjection) {
				ev := *p.Ops[len(p.Ops)-1].Event
				ev.Status = "shrugged"
				p.Ops[len(p.Ops)-1].Event = &ev
			}),
			want: "unknown terminal status",
		},
		{
			name: "missing run.started",
			proj: mutate(func(p *JournalProjection) {
				p.Ops = p.Ops[1:]
			}),
			want: "first op is not the run.started",
		},
		{
			name: "empty ops",
			proj: mutate(func(p *JournalProjection) {
				p.Ops = nil
			}),
			want: "no journal ops",
		},
		{
			name: "missing pinned workflow definition",
			proj: mutate(func(p *JournalProjection) {
				p.Definition = nil
			}),
			want: "no pinned workflow definition",
		},
		{
			name: "gate verdict references unrecorded artifact",
			proj: mutate(func(p *JournalProjection) {
				at := p.Ops[1].Time
				ghost := journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: wf.TerminalComplete, Name: "verdict/review-0.json"}
				terminal := p.Ops[len(p.Ops)-1]
				p.Ops = append(append(p.Ops[:len(p.Ops)-1:len(p.Ops)-1], JournalOp{Kind: opAppend, Event: &ghost, Time: at}), terminal)
			}),
			want: "unrecorded artifact",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), tc.proj)
			if err == nil {
				t.Fatalf("ProjectRun accepted an unprojectable history")
			}
			if !errors.Is(err, ErrUnprojectable) && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrUnprojectable mentioning %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// TestProjectionOpTimesAreWorkflowClock guards against wall-clock leakage: op
// times come from workflow.Now, so replaying the same fixture yields the same
// projection (and therefore the same journal bytes).
func TestProjectionOpTimesAreWorkflowClock(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	newProj := func() JournalProjection {
		return executeForProjection(t, projectionInput("proj-clock", spec), &Activities{
			Det:        &scriptedStages{},
			Workspaces: testWorkspaces(t),
		}, false)
	}
	a, b := newProj(), newProj()
	if len(a.Ops) != len(b.Ops) {
		t.Fatalf("op counts differ: %d vs %d", len(a.Ops), len(b.Ops))
	}
	for i := range a.Ops {
		if !a.Ops[i].Time.Equal(b.Ops[i].Time) {
			t.Errorf("op %d time differs across identical executions: %v vs %v", i, a.Ops[i].Time, b.Ops[i].Time)
		}
	}
	var zero time.Time
	for i, op := range a.Ops {
		if op.Time.Equal(zero) {
			t.Errorf("op %d has a zero time", i)
		}
	}
}

type mutationSidecarDeterministic struct {
	line   string
	status apiv1.ResultStatus
}

func (d mutationSidecarDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if err := os.WriteFile(filepath.Join(env.Workspace, mutationsSidecarFile), []byte(d.line+"\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	status := d.status
	if status == "" {
		status = apiv1.ResultSuccess
	}
	return apiv1.ResultEnvelope{Status: status}, nil
}

func TestProjectionCarriesProviderMutationProvenance(t *testing.T) {
	spec := crSpec("mutate", []apiv1.Task{crTask("mutate", "")}, nil)
	spec.Tasks[0].Run.Workspace = apiv1.WorkspaceRepo
	proj := executeForProjection(t, projectionInput("mutation-provenance", spec), &Activities{
		Det: mutationSidecarDeterministic{
			line:   `{"provider":"github","kind":"issue","id":"629","url":"https://github.com/Agent-Clubhouse/Goobers/issues/629","operation":"update"}`,
			status: apiv1.ResultNoWork,
		},
		Workspaces: testWorkspaces(t),
	}, false)

	var branch, mutation *journal.Event
	for _, op := range proj.Ops {
		if op.Event == nil || op.Event.Type != journal.EventRefTouched {
			continue
		}
		if op.Event.Stage == "mutate" {
			mutation = op.Event
		} else if op.Event.ExternalRef != nil && op.Event.ExternalRef.Kind == "branch" {
			branch = op.Event
		}
	}
	if branch == nil {
		t.Fatal("mutating no-work result did not record run-branch provenance")
	}
	if mutation == nil || mutation.ExternalRef == nil {
		t.Fatalf("projected ops carry no stage mutation: %+v", proj.Ops)
	}
	if got := *mutation.ExternalRef; got.Provider != "github" || got.Kind != "issue" || got.ID != "629" {
		t.Fatalf("mutation ref = %+v", got)
	}
	if mutation.Runner["operation"] != "update" {
		t.Fatalf("mutation runner annotation = %+v", mutation.Runner)
	}
}

func TestProjectionMalformedMutationProvenanceIsNonFatal(t *testing.T) {
	spec := crSpec("mutate", []apiv1.Task{crTask("mutate", "")}, nil)
	proj := executeForProjection(t, projectionInput("invalid-mutation", spec), &Activities{
		Det:        mutationSidecarDeterministic{line: `{"provider":"github","kind":"issue"}`},
		Workspaces: testWorkspaces(t),
	}, false)

	var issue *journal.Event
	for _, op := range proj.Ops {
		if op.Event == nil {
			continue
		}
		if op.Event.Type == journal.EventRefTouched && op.Event.Stage == "mutate" {
			t.Fatalf("malformed mutation fabricated provenance: %+v", op.Event)
		}
		if op.Event.Type == journal.EventError && op.Event.Error != nil &&
			op.Event.Error.Code == "mutation_sidecar_read_failed" {
			issue = op.Event
		}
	}
	if issue == nil || issue.Stage != "mutate" {
		t.Fatalf("malformed mutation issue was not surfaced: %+v", proj.Ops)
	}
	last := proj.Ops[len(proj.Ops)-1].Event
	if last == nil || last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseCompleted) {
		t.Fatalf("malformed provenance changed the run outcome: %+v", last)
	}
}

func TestProjectionScrubsVerdictBeforeHistoryAndPointerAddressing(t *testing.T) {
	const secret = "history-secret-value"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))

	var mu sync.Mutex
	var implementEnvs []apiv1.InvocationEnvelope
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			mu.Lock()
			implementEnvs = append(implementEnvs, env)
			mu.Unlock()
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			mu.Lock()
			defer mu.Unlock()
			reviews++
			if reviews == 1 {
				return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "remove " + secret}, nil
			}
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	proj := executeForProjection(t, projectionInput("scrubbed-verdict", gatedSpec()), &Activities{
		Goober: inv, Workspaces: testWorkspaces(t), Scrubber: registry,
	}, false)
	history, err := json.Marshal(proj)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	if strings.Contains(string(history), secret) {
		t.Fatalf("secret survived in Temporal-derived projection history: %s", history)
	}

	mu.Lock()
	repass := append([]apiv1.InvocationEnvelope(nil), implementEnvs...)
	mu.Unlock()
	if len(repass) != 2 {
		t.Fatalf("implement dispatches = %d, want one repass", len(repass))
	}
	pointer := findContextPointer(repass[1].ContextPointers, "review.verdict")
	if pointer == nil || pointer.Artifact == nil {
		t.Fatalf("repass context = %+v, want verdict artifact", repass[1].ContextPointers)
	}
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	data, err := pointer.Artifact.Resolve(dir)
	if err != nil {
		t.Fatalf("resolve scrubbed verdict pointer: %v", err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), journal.Redacted) {
		t.Fatalf("projected verdict was not scrubbed: %s", data)
	}
}
