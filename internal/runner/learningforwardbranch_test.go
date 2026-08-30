package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// The #3929 ruling in one table: a learning episode is injected IFF the gate
// result's repass attempt is at least 1. Nothing else — not the failure class,
// not the target's identity, not whether the target happens to be upstream in
// some independently re-derived sense — participates.
//
// This is the single predicate BOTH drivers call (internal/engine's gate arm
// and this package's stepGate), which is the whole point of hoisting it here:
// the engine used to re-derive "does this gate send work back?" from its own
// upstream map, and a second derivation is a second thing to drift.
func TestLearningEpisodeAppliesToRepassIsTheRuling(t *testing.T) {
	for _, tt := range []struct {
		attempt int
		want    bool
		why     string
	}{
		{attempt: 0, want: false, why: "a forward branch: the gate routed to a stage that has not run, so there is nothing to correct"},
		{attempt: 1, want: true, why: "the first true repass"},
		{attempt: 2, want: true, why: "a later repass, still within the budget"},
		{attempt: gate.DefaultMaxRepasses, want: true, why: "the last repass the policy budget allows"},
		{attempt: -1, want: false, why: "never produced by trackRepass, but must not be read as a repass if it ever were"},
	} {
		t.Run(fmt.Sprintf("attempt=%d", tt.attempt), func(t *testing.T) {
			if got := LearningEpisodeAppliesToRepass(tt.attempt); got != tt.want {
				t.Fatalf("LearningEpisodeAppliesToRepass(%d) = %v, want %v — %s", tt.attempt, got, tt.want, tt.why)
			}
		})
	}
}

// TestForwardBranchKeepsItsRetryDecisionWithoutAnEpisode is the runner-side
// regression for #3929, driven through a real Start() rather than through the
// helper in isolation.
//
// The ruling narrows ONE thing: whether an episode is injected. It explicitly
// does not narrow retry CLASSIFICATION, the retry-decision annotation, what
// routeRetryDecision returns, or the routing itself — those are what the
// dispositions surface, the repass budget and the E2 parity row all select on,
// and silently dropping them while fixing the episode would have traded one
// divergence for a worse one.
//
// So this pins the whole retry arm on a forward branch: implement fails with a
// classifiable nonzero_exit, the status-equals gate fails it, the fail branch
// routes ONWARD to park-needs-human (a stage that has never run), and:
//
//   - the retry decision is still journalled, still classified policy, still
//     carries the subject's failure code, still names park-needs-human as its
//     target, and carries repassAttempt 0 — the evidence the ruling reads;
//   - park-needs-human is still dispatched, so ws.state took the branch;
//   - NO learning episode is built, journalled, or pointed at.
func TestForwardBranchKeepsItsRetryDecisionWithoutAnEpisode(t *testing.T) {
	const runID = "run-forward-branch-no-episode"
	forwardEnv := make(chan apiv1.InvocationEnvelope, 1)
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stageCapturingDeterministic{
			suffix: ":park-needs-human",
			env:    forwardEnv,
			delegate: &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":implement": {
					status:  apiv1.ResultFailure,
					summary: "the build did not compile",
					errorInfo: &apiv1.ErrorInfo{
						Code: "nonzero_exit", Message: "exit status 1", Retryable: true,
					},
				},
				runID + ":park-needs-human": {status: apiv1.ResultSuccess},
			}},
		}, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: escalationParkingMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted (park-needs-human terminates @abort)", res.Phase)
	}

	events := readJournalEvents(t, filepath.Join(runsDir, runID))

	// (1) Routing is unchanged: the forward branch was actually taken.
	if !stageStarted(events, "park-needs-human") {
		t.Fatal("park-needs-human never started — gating the episode must not gate the branch")
	}
	if starts := countStageStarts(events, "implement"); starts != 1 {
		t.Fatalf("implement stage.started = %d, want exactly 1 (a forward branch is not a repass)", starts)
	}

	// (2) The retry decision itself is unchanged, repassAttempt included.
	decisions := forwardRetryDecisions(events)
	if len(decisions) != 1 {
		t.Fatalf("retry decision annotations = %d (%+v), want exactly 1", len(decisions), decisions)
	}
	got := decisions[0]
	want := runnerRetryDecision{
		Stage: "implement", Gate: "review",
		FailureClass:  string(journal.AttemptPolicy),
		FailureCode:   "nonzero_exit",
		RepassAttempt: 0, Target: "park-needs-human",
	}
	if got != want {
		t.Fatalf("retry decision = %+v, want %+v — the ruling gates the EPISODE, never the decision", got, want)
	}

	// (3) No episode: no annotation, no artifact, no pointer.
	if n := countLearningInjections(events); n != 0 {
		t.Fatalf("learning episode injections = %d, want 0 — park-needs-human has never run, so it produced nothing to correct", n)
	}
	for _, e := range events {
		if strings.HasPrefix(e.Name, "learning/episode-") {
			t.Fatalf("seq %d named a learning episode artifact %q on a forward branch", e.Seq, e.Name)
		}
	}

	// And nothing reached the forward stage's own inputs: the disposition
	// stage is dispatched with the verdict, never with a fabricated correction
	// addressed to work it has not done.
	var env apiv1.InvocationEnvelope
	select {
	case env = <-forwardEnv:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("the park-needs-human invocation was never captured")
	}
	for _, pointer := range env.ContextPointers {
		if strings.HasPrefix(pointer.Name, "learning.episode[") {
			t.Fatalf("park-needs-human was handed an episode pointer %q", pointer.Name)
		}
	}
}

// TestParallelForwardBranchLeavesBranchAccountingIntact is the parallel
// accounting guard.
//
// stepGate's retry arm is shared by the sequential walk and by every branch of
// a sequentially-executed parallel (ws.parallel != nil), and inside a branch it
// routes the injected pointers through the BRANCH pointer set rather than the
// run's. Gating the injection therefore runs the risk of settling a branch
// differently — of skipping ws.state = retryTarget, of leaving a branch
// unsettled, or of perturbing the completeness record a join reads.
//
// This walks a two-branch parallel where branch a's gate routes ONWARD on a
// retryable failure and pins, exactly:
//
//   - the completeness record, flattened in declaration order;
//   - both branch.finished statuses;
//   - that the forward stage inside the branch ran and the join was reached;
//   - that no episode was injected and no episode pointer reached the join.
//
// The bound is deliberately left unset, so this fixture exercises the
// sequential path stepGate owns. Its concurrent counterpart is #3932's
// TestConcurrentForwardBranchInjectsNothing in learningconcurrentbranch_test.go:
// both arms now share one producer (recordGateRetryInjection), so the ruling is
// stated once and tested on both walkers rather than only on this one.
func TestParallelForwardBranchLeavesBranchAccountingIntact(t *testing.T) {
	const runID = "run-parallel-forward-branch"
	joinEnv := make(chan apiv1.InvocationEnvelope, 1)
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &collateCapturingDeterministic{
			env: joinEnv,
			delegate: &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":lens-a": {
					status:  apiv1.ResultFailure,
					summary: "the lens could not complete",
					errorInfo: &apiv1.ErrorInfo{
						Code: "nonzero_exit", Message: "exit status 1", Retryable: true,
					},
				},
				runID + ":park-a":  {status: apiv1.ResultSuccess},
				runID + ":lens-b":  {status: apiv1.ResultSuccess},
				runID + ":collate": {status: apiv1.ResultSuccess},
			}},
		}, nil
	}, gate.NewAutomatedEvaluator())
	r.cfg.ScratchDir = t.TempDir()

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: parallelForwardBranchMachine(t),
		Gaggle:  "demo",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	events := readJournalEvents(t, filepath.Join(runsDir, runID))

	if !stageStarted(events, "park-a") {
		t.Fatal("park-a never started — the forward branch inside a parallel must still be taken")
	}
	if !stageStarted(events, "collate") {
		t.Fatal("collate never started — branch a must still settle and reach the join")
	}

	// The completeness record is the parallel's meaning; find the event that
	// carries it.
	var finished *journal.Event
	for i := range events {
		if events[i].Type == journal.EventParallelFinished {
			finished = &events[i]
		}
	}
	if finished == nil {
		t.Fatal("no parallel.finished event — the parallel never settled")
	}
	// The completeness record is the parallel's meaning. Pin it flattened, in
	// declaration order, so any change to how a forward-branching branch
	// settles shows up here rather than in a join's inputs three stages later.
	// Both branches settle no-output because these stub stages record no
	// artifacts — the load-bearing part is that branch a, whose gate routed
	// ONWARD, settles exactly like its ordinary sibling. This is not a
	// formality: the injected episode is itself a recorded artifact, so before
	// the ruling branch a settled "succeeded" purely on the strength of the
	// fabricated correction. A join that switches on completeness was reading
	// a branch's output count off work the branch never did.
	if got, want := flattenCompleteness(finished.Completeness), "1:a:no-output;2:b:no-output"; got != want {
		t.Fatalf("completeness = %q, want %q", got, want)
	}
	for _, e := range events {
		if e.Type != journal.EventBranchFinished {
			continue
		}
		switch e.BranchStatus {
		case journal.BranchFailed, journal.BranchCancelled, journal.BranchTimedOut:
			t.Fatalf("branch %q finished %q — gating the episode must not change how a branch settles",
				e.BranchName, e.BranchStatus)
		}
	}

	// The retry decision inside the branch survives, with repassAttempt 0.
	decisions := forwardRetryDecisions(events)
	if len(decisions) != 1 {
		t.Fatalf("retry decision annotations = %d (%+v), want exactly 1", len(decisions), decisions)
	}
	want := runnerRetryDecision{
		Stage: "lens-a", Gate: "gate-a",
		FailureClass:  string(journal.AttemptPolicy),
		FailureCode:   "nonzero_exit",
		RepassAttempt: 0, Target: "park-a",
	}
	if decisions[0] != want {
		t.Fatalf("retry decision = %+v, want %+v", decisions[0], want)
	}

	if n := countLearningInjections(events); n != 0 {
		t.Fatalf("learning episode injections = %d, want 0 inside a forward-branching parallel branch", n)
	}
	for _, e := range events {
		if strings.HasPrefix(e.Name, "learning/episode-") {
			t.Fatalf("seq %d named a learning episode artifact %q inside a forward-branching parallel branch", e.Seq, e.Name)
		}
	}

	// The join must not be handed a correction either: a branch-scoped pointer
	// that is never created cannot leak, but pinning it here is what keeps the
	// two claims separable — "no episode was injected" and "no episode
	// escaped the branch" fail in different places.
	var env apiv1.InvocationEnvelope
	select {
	case env = <-joinEnv:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("the join invocation was never captured")
	}
	for _, pointer := range env.ContextPointers {
		if strings.HasPrefix(pointer.Name, "learning.episode[") {
			t.Fatalf("the join was handed an episode pointer %q from a forward branch", pointer.Name)
		}
	}
}

// --- fixtures and readers ---

// stageCapturingDeterministic tees the invocation envelope of one named stage
// so a test can assert on what that stage was actually HANDED, which is the
// only place a fabricated correction would become visible to a goober.
type stageCapturingDeterministic struct {
	suffix   string
	delegate invoke.Deterministic
	env      chan<- apiv1.InvocationEnvelope
}

func (c *stageCapturingDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if strings.HasSuffix(env.TaskID, c.suffix) {
		select {
		case c.env <- env:
		default:
		}
	}
	return c.delegate.Run(ctx, env, run)
}

// parallelForwardBranchMachine is a two-branch parallel whose first branch
// carries a status-equals gate routing ONWARD (park-a) rather than back to
// lens-a. maxConcurrentBranches is deliberately left unset so the sequential
// walker — the one stepGate's retry arm serves — executes the branches.
func parallelForwardBranchMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "parallel-forward-branch", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("lens-a", "gate-a"),
				task("park-a", workflow.TargetJoin),
				task("lens-b", workflow.TargetJoin),
				task("collate", workflow.TerminalComplete),
			},
			Gates: []apiv1.Gate{{
				Name:      "gate-a",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass": workflow.TargetJoin,
					"fail": "park-a",
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				Join: "collate",
				Branches: []apiv1.Branch{
					{Name: "a", Start: "lens-a"},
					{Name: "b", Start: "lens-b"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile parallel forward-branch fixture: %v", err)
	}
	return machine
}

// runnerRetryDecision is the comparable projection of a stage.retry.decision
// annotation. It is deliberately the WHOLE annotation shape, so a change that
// drops or rewrites any field fails here rather than silently.
type runnerRetryDecision struct {
	Stage         string
	Gate          string
	FailureClass  string
	FailureCode   string
	RepassAttempt int
	Target        string
}

func forwardRetryDecisions(events []journal.Event) []runnerRetryDecision {
	out := []runnerRetryDecision{}
	for _, e := range events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner == nil {
			continue
		}
		if kind, _ := e.Runner["kind"].(string); kind != retryDecisionKind {
			continue
		}
		attempt, _ := runnerInt(e.Runner["repassAttempt"])
		class, _ := e.Runner[retryFailureClassKey].(string)
		code, _ := e.Runner["failureCode"].(string)
		target, _ := e.Runner["target"].(string)
		out = append(out, runnerRetryDecision{
			Stage: e.Stage, Gate: e.Gate, FailureClass: class,
			FailureCode: code, RepassAttempt: attempt, Target: target,
		})
	}
	return out
}

func countLearningInjections(events []journal.Event) int {
	n := 0
	for _, e := range events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner == nil {
			continue
		}
		if kind, _ := e.Runner["kind"].(string); kind == LearningEpisodeInjectedKind {
			n++
		}
	}
	return n
}

func stageStarted(events []journal.Event, stage string) bool {
	return countStageStarts(events, stage) > 0
}

func countStageStarts(events []journal.Event, stage string) int {
	n := 0
	for _, e := range events {
		if e.Type == journal.EventStageStarted && e.Stage == stage {
			n++
		}
	}
	return n
}

func flattenCompleteness(record []journal.BranchOutcome) string {
	out := ""
	for i, outcome := range record {
		if i > 0 {
			out += ";"
		}
		out += fmt.Sprintf("%d:%s:%s", outcome.Branch, outcome.Name, outcome.Status)
	}
	return out
}
