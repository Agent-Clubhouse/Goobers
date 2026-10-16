package runner

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type childOriginGoober struct {
	mu        sync.Mutex
	envelopes []apiv1.InvocationEnvelope
	failFirst bool
}

func (g *childOriginGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.envelopes = append(g.envelopes, env)
	if g.failFirst && len(g.envelopes) == 1 {
		return apiv1.ResultEnvelope{}, errors.New("retry the attempt")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (*childOriginGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	panic("unexpected reviewer")
}

// Exercise the production runTask/dispatchTask envelope path directly: public
// start/resume entry points must keep refusing child execution until wired.
func childOriginRuntime(t *testing.T, goober *childOriginGoober) (*Runner, *journal.Run, taskFrame) {
	t.Helper()
	task := apiv1.Task{Name: "work", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder", Workspace: apiv1.WorkspaceScratch,
		Retry: &apiv1.RetryPolicy{MaxAttempts: 5}, ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}
	machine, err := workflow.Compile(workflow.Definition{Name: "children", Version: 1, DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: task.Name, Tasks: []apiv1.Task{task},
	}}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{RunID: "origin-run", Workflow: "children", Gaggle: "web", WorkflowDigest: machine.Digest()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	cfg := Config{RunsDir: root, ScratchDir: filepath.Join(t.TempDir(), "scratch"), NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return goober, nil }}
	runner := &Runner{cfg: cfg}
	task, _ = machine.Task(task.Name)
	frame := taskFrame{jr: run, in: StartInput{RunID: "origin-run", Gaggle: "web", Machine: machine}, t: task, ex: childOriginExecutors(cfg, run)}
	return runner, run, frame
}

func childOriginExecutors(cfg Config, run *journal.Run) *executors {
	registry, _ := journal.DefaultScrubber()
	return newExecutors(cfg, run, registry)
}

func TestChildOriginRuntimeRetryRevisitRecoveryAndHumanRestart(t *testing.T) {
	goober := &childOriginGoober{failFirst: true}
	r, run, frame := childOriginRuntime(t, goober)
	execute := func(attempt int32, class journal.AttemptClass, rerun *rerunContext) {
		t.Helper()
		if _, _, err := r.runTask(t.Context(), frame, 0, attempt, class, "", rerun, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	execute(1, "", nil)
	first, retry := goober.envelopes[0].ChildWorkflowOrigin, goober.envelopes[1].ChildWorkflowOrigin
	if first == nil || retry == nil || first.StageOccurrence != retry.StageOccurrence || first.AttemptID == retry.AttemptID {
		t.Fatalf("retry origins: %+v %+v", first, retry)
	}
	execute(1, "", nil)
	visit := goober.envelopes[2].ChildWorkflowOrigin
	if visit.StageOccurrence == first.StageOccurrence || visit.StageOccurrence != visit.AttemptID {
		t.Fatalf("revisit reused origin: %+v", visit)
	}
	// A crash after stage.started but before invocation is recovered through
	// the durable binding even though the taskFrame object no longer exists.
	if err := frame.recordTaskStarted(2, journal.AttemptInfra); err != nil {
		t.Fatal(err)
	}
	dir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	frame.jr, frame.childOrigin = recovered, nil
	frame.ex = childOriginExecutors(r.cfg, recovered)
	execute(3, journal.AttemptInfra, nil)
	resumed := goober.envelopes[3].ChildWorkflowOrigin
	if resumed.StageOccurrence != visit.StageOccurrence || resumed.AttemptID == visit.AttemptID {
		t.Fatalf("resumed origin: %+v", resumed)
	}
	if err := recovered.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Append(journal.Event{Type: journal.EventStageRerunRequested, Stage: "work", Attempt: 4, AttemptClass: journal.AttemptHuman}); err != nil {
		t.Fatal(err)
	}
	execute(4, journal.AttemptHuman, &rerunContext{stage: "work", attempt: 4, requestAttempt: 4})
	human := goober.envelopes[4].ChildWorkflowOrigin
	if human.StageOccurrence != visit.StageOccurrence || human.AttemptID == resumed.AttemptID {
		t.Fatalf("human restart origin: %+v", human)
	}
	assertChildEnvelopesMatchJournal(t, recovered.Dir(), goober.envelopes)
}

func TestChildOriginRuntimeParallelSiblingsAndBranchRevisit(t *testing.T) {
	goober := &childOriginGoober{}
	r, run, frame := childOriginRuntime(t, goober)
	var group sync.WaitGroup
	failures := make(chan error, 2)
	for branch := 1; branch <= 2; branch++ {
		group.Add(1)
		go func() {
			defer group.Done()
			sibling := frame
			sibling.jr = &branchJournal{run: run, branch: branch}
			sibling.ex = childOriginExecutors(r.cfg, run)
			for visit := 0; visit < 2; visit++ {
				if _, _, err := r.runTask(t.Context(), sibling, branch, 1, "", "", nil, false, nil); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	seen := map[string]bool{}
	for _, env := range goober.envelopes {
		if env.ChildWorkflowOrigin == nil || seen[env.ChildWorkflowOrigin.StageOccurrence] {
			t.Fatalf("sibling/revisit reused occurrence: %+v", env.ChildWorkflowOrigin)
		}
		seen[env.ChildWorkflowOrigin.StageOccurrence] = true
	}
	if len(seen) != 4 {
		t.Fatalf("occurrences=%d, want 4", len(seen))
	}
	assertChildEnvelopesMatchJournal(t, run.Dir(), goober.envelopes)
}

func assertChildEnvelopesMatchJournal(t *testing.T, dir string, envelopes []apiv1.InvocationEnvelope) {
	t.Helper()
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]apiv1.ChildWorkflowOrigin{}
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			origin, err := journal.ChildWorkflowOriginForEvent("origin-run", event)
			if err != nil {
				t.Fatal(err)
			}
			starts[origin.AttemptID] = *origin
		}
	}
	for _, env := range envelopes {
		if env.ChildWorkflowOrigin == nil || starts[env.ChildWorkflowOrigin.AttemptID] != *env.ChildWorkflowOrigin || env.Goober != "coder" {
			t.Fatalf("invocation lost committed origin or owner: %+v", env)
		}
	}
}

func TestChildOriginAbsentForOrdinaryTask(t *testing.T) {
	run := newRunnerTestJournal(t, "ordinary")
	frame := taskFrame{jr: run, t: apiv1.Task{Name: "work", Type: apiv1.TaskAgentic, Goober: "coder"}}
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	var env apiv1.InvocationEnvelope
	frame.pinPublicationAuthority(&env, 1)
	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	started := events[len(events)-1]
	if env.ChildWorkflowOrigin != nil || len(started.Runner) != 1 || started.Runner["goober"] != "coder" {
		t.Fatalf("ordinary task acquired child metadata: %+v %+v", env, started)
	}
}
