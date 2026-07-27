package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// executeForProjection runs one engine fixture in the Temporal test
// environment and returns its queried journal projection. wantWorkflowErr
// tolerates a failed workflow — a failed run still projects (the projection is
// a function of history, not of success).
func executeForProjection(t *testing.T, in RunInput, acts *Activities, wantWorkflowErr bool) JournalProjection {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
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
