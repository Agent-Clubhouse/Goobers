package engine

// The dual-runner conformance harness (#40/#637): shared workflow fixtures run
// through BOTH the local runner (the behavioral reference) and this engine
// (hosted in Temporal's test environment with the cloud worker's live journal),
// and the two journals are diffed over
// journal.ConformanceView — the single sanctioned comparison surface.
// Timestamps, durations, infra-retry attempts, and runner.* fields are
// excluded by the shared view itself, never by ad-hoc harness filtering.
//
// These tests are part of `make test-conformance` (the target runs every
// ^TestConformance test hermetically). Each fixture targets a #156 divergence
// class. A fixture exposing a real, unfixed divergence must land skipped with
// a linked follow-up — never paper over the diff by weakening the view.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/testgit"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// flakyAutomated wraps a real automated evaluator, failing the first
// transientFailures calls with an infrastructure-marked error (#765's class).
type flakyAutomated struct {
	mu                sync.Mutex
	inner             invoke.Automated
	transientFailures int
	calls             int
}

func (f *flakyAutomated) Evaluate(ctx context.Context, g apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	f.mu.Lock()
	f.calls++
	failNow := f.calls <= f.transientFailures
	f.mu.Unlock()
	if failNow {
		return "", invoke.InfrastructureFailure(errors.New("evaluator worker lost"))
	}
	return f.inner.Evaluate(ctx, g, env)
}

// --- fixture corpus -------------------------------------------------------

// conformanceFixture is one shared workflow fixture: a definition plus
// scripted stage behavior, executed identically through both runners.
type conformanceFixture struct {
	name string
	spec apiv1.WorkflowSpec
	// script keys are stage names; entries play per dispatch (see scriptedExec).
	script map[string][]scriptedCall
	// gateTransientFailures makes the automated evaluator infra-fail this many
	// times before answering (exercises #765's evaluator retry on both sides).
	gateTransientFailures int
	maxRepasses           int
	// wantLocalErr / wantEngineErr accept the failure-class fixtures: the
	// local runner returns the walk error, the engine fails the workflow.
	// Journals are still produced and compared either way.
	wantLocalErr  bool
	wantEngineErr bool
	// usesRepo marks fixtures whose workflow touches repository workspaces
	// (agentic stages) and therefore needs the hermetic git fixture repo on
	// the local side.
	usesRepo bool
	// Optional observers/evaluators exercise the actual gate envelopes on
	// both runners, including protected host-authored join inputs.
	automated invoke.Automated
	goober    invoke.Goober
}

func failureClassGate(name string, branches map[string]string) apiv1.Gate {
	return apiv1.Gate{
		Name: name, Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "failure-class"},
		Branches:  branches,
	}
}

func fail(code, message string) scriptedCall {
	return scriptedCall{result: apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: code, Message: message},
	}}
}

// failRetryable is a failure the producer itself marked retryable. It is what
// gate.AutomatedInputs flattens into errorRetryable, which is what the
// "failure-class" check reads to answer gate.OutcomeInfra — the signal a real
// lane uses to say "the work did not fail, the machinery did".
func failRetryable(code, message string) scriptedCall {
	return scriptedCall{result: apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: code, Message: message, Retryable: true},
	}}
}

func succeed(outputs map[string]interface{}) scriptedCall {
	return scriptedCall{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: outputs}}
}

func conformanceFixtures() []conformanceFixture {
	reviewLoop := fixtureSpec("implement",
		[]apiv1.Task{detTask("implement", "review")},
		[]apiv1.Gate{statusGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": "implement"})})

	return []conformanceFixture{
		{
			name: "failure-class infra retry recovers",
			spec: fixtureSpec("implement", []apiv1.Task{detTask("implement", "review")},
				[]apiv1.Gate{failureClassGate("review", map[string]string{
					"pass": wf.TerminalComplete, "fail": wf.TargetAbort, "infra": "implement",
				})}),
			script: map[string][]scriptedCall{
				"implement": {failRetryable("worktree_provision_failed", "workspace host unavailable"), succeed(nil)},
			},
			maxRepasses: 3,
		},
		{
			// Walking-skeleton class: multi-stage sequence mixing an agentic
			// stage (capability-carrying invocation) with a deterministic
			// stage, through a passing gate. Exercises ref.touched run-branch
			// provenance (repo-using machine, manual trigger).
			name: "multi-stage agentic and deterministic with gate pass",
			spec: fixtureSpec("implement",
				[]apiv1.Task{
					agenticTask("implement", "review", "code:write"),
					detTask("local-ci", ""),
				},
				[]apiv1.Gate{statusGate("review", map[string]string{"pass": "local-ci", "fail": wf.TargetAbort})}),
			script: map[string][]scriptedCall{
				"implement": {succeed(map[string]interface{}{"changedFileCount": 1})},
			},
			usesRepo: true,
		},
		{
			name: "gate repass then pass",
			spec: reviewLoop,
			script: map[string][]scriptedCall{
				"implement": {fail("build_failed", "first try fails"), succeed(nil)},
			},
			maxRepasses: 3,
		},
		{
			name: "gate repass exhaustion escalates",
			spec: reviewLoop,
			script: map[string][]scriptedCall{
				"implement": {fail("build_failed", "always fails")},
			},
			maxRepasses: 1,
		},
		{
			name: "escalation routes through the escalate control branch",
			spec: fixtureSpec("implement",
				[]apiv1.Task{detTask("implement", "review"), detTask("park-escalated", wf.TargetEscalate)},
				[]apiv1.Gate{statusGate("review", map[string]string{
					"pass": wf.TerminalComplete, "fail": "implement", wf.BranchEscalate: "park-escalated",
				})}),
			script: map[string][]scriptedCall{
				"implement": {fail("build_failed", "always fails")},
			},
			maxRepasses: 1,
		},
		{
			name: "policy retry exhaustion fails the run",
			spec: fixtureSpec("implement",
				[]apiv1.Task{func() apiv1.Task {
					t := detTask("implement", "")
					t.Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
					return t
				}()}, nil),
			script: map[string][]scriptedCall{
				"implement": {
					{err: errors.New("tool exploded")},
					{err: errors.New("tool exploded again")},
				},
			},
			wantLocalErr:  true,
			wantEngineErr: true,
		},
		{
			name: "infra retry recovers and completes",
			spec: fixtureSpec("implement", []apiv1.Task{detTask("implement", "")}, nil),
			script: map[string][]scriptedCall{
				"implement": {
					{err: invoke.InfrastructureFailure(errors.New("workspace host vanished"))},
					succeed(nil),
				},
			},
		},
		{
			// #622/#724 timeout classification: a stage overrunning its
			// declared duration limit surfaces at the invoke seam as
			// invoke.Timeout, a POLICY-classed failure on both runners — the
			// worker self-enforces the limit, and the engine's
			// StartToCloseTimeout runs stageTimeoutGrace behind it so
			// Temporal's worker-loss timeout never fires first. Same
			// definition, same retry budget, same terminal journal.
			name: "worker-enforced stage timeout retries as policy",
			spec: fixtureSpec("implement",
				[]apiv1.Task{func() apiv1.Task {
					t := detTask("implement", "")
					t.TimeoutSeconds = 45
					t.Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
					return t
				}()}, nil),
			script: map[string][]scriptedCall{
				"implement": {
					{err: invoke.Timeout(errors.New("stage exceeded its declared duration limit"))},
					succeed(nil),
				},
			},
		},
		{
			// #544: blocked is a schema-valid producer value, mapped to the
			// escalated terminal with its cause journaled — it short-circuits
			// before any Next state, so the fixture needs no gate.
			name: "blocked halts at the escalated terminal",
			spec: fixtureSpec("implement", []apiv1.Task{detTask("implement", "")}, nil),
			script: map[string][]scriptedCall{
				"implement": {{result: apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Summary: "needs a human"}}},
			},
		},
		{
			name: "non-gate failure fails the run with its cause",
			spec: fixtureSpec("implement", []apiv1.Task{detTask("implement", "")}, nil),
			script: map[string][]scriptedCall{
				"implement": {fail("boom_code", "it broke")},
			},
		},
		{
			name: "no-work short-circuits to completed",
			spec: fixtureSpec("implement", []apiv1.Task{detTask("implement", "next-stage"), detTask("next-stage", "")}, nil),
			script: map[string][]scriptedCall{
				"implement": {{result: apiv1.ResultEnvelope{Status: apiv1.ResultNoWork, Summary: "empty tick"}}},
			},
		},
		{
			name: "tolerated failure advances and stays visible",
			spec: fixtureSpec("implement", []apiv1.Task{
				func() apiv1.Task {
					t := detTask("implement", "deploy")
					t.ContinueOnError = true
					return t
				}(),
				detTask("deploy", ""),
			}, nil),
			script: map[string][]scriptedCall{
				"implement": {fail("soft", "tolerated")},
			},
		},
		{
			name: "transient gate evaluator failure retries within its bound",
			spec: fixtureSpec("implement",
				[]apiv1.Task{detTask("implement", "review")},
				[]apiv1.Gate{{
					Name: "review", Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "status-equals", Retry: &apiv1.RetryPolicy{MaxAttempts: 2}},
					Branches:  map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort},
				}}),
			gateTransientFailures: 1,
		},
	}
}

// --- harness ----------------------------------------------------------------

// newConformanceFixtureRepo builds the hermetic local git repo agentic-stage
// fixtures clone from (no network — the same pattern as the walking skeleton).
func newConformanceFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runFixtureGit(t, work, "init", "--initial-branch=main")
	runFixtureGit(t, work, "config", "user.email", "test@example.com")
	runFixtureGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, work, "add", "README.md")
	runFixtureGit(t, work, "commit", "-m", "initial")
	runFixtureGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runFixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

// runLocalFixture executes fx through the real local runner and returns the
// run's journal events.
func runLocalFixture(t *testing.T, fx conformanceFixture, runID string) []journal.Event {
	t.Helper()
	exec := newScriptedExec(fx.script)
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	cfg := runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return exec, nil
		},
		NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			if fx.goober != nil {
				return fx.goober, nil
			}
			return exec, nil
		},
		Automated:   &flakyAutomated{inner: gate.NewAutomatedEvaluator(), transientFailures: fx.gateTransientFailures},
		MaxRepasses: fx.maxRepasses,
		Worktrees:   wtMgr,
		RunsDir:     runsDir,
		ScratchDir:  filepath.Join(instanceRoot, "scratch"),
	}
	if fx.automated != nil {
		cfg.Automated = &flakyAutomated{inner: fx.automated, transientFailures: fx.gateTransientFailures}
	}
	if fx.usesRepo {
		repo := newConformanceFixtureRepo(t)
		cfg.RepoCloneURL = func(apiv1.RepoRef) (string, error) { return repo, nil }
	}
	r, err := runner.New(cfg)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	machine, err := wf.Compile(
		wf.Definition{Name: "conformance", Version: 1, Spec: fx.spec},
		wf.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = r.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if (err != nil) != fx.wantLocalErr {
		t.Fatalf("local Start error = %v, wantLocalErr = %t", err, fx.wantLocalErr)
	}
	return readJournalEvents(t, filepath.Join(runsDir, runID))
}

// runEngineFixture executes fx through the engine in the Temporal test
// environment with a real live journal writer, returning its durable events.
// It does not depend on the removed local mode-3 host/projection APIs.
func runEngineFixture(t *testing.T, fx conformanceFixture, runID string) []journal.Event {
	t.Helper()
	exec := newScriptedExec(fx.script)
	writer, runsDir := newLiveWriter(t)
	in := RunInput{
		RunID:                  runID,
		LiveJournal:            true,
		Gaggle:                 "web",
		WorkflowName:           "conformance",
		Version:                1,
		PreviewFeaturesEnabled: boolPointer(true),
		Spec:                   fx.spec,
		RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		TriggerKind:            string(journal.TriggerManual),
		MaxRepasses:            fx.maxRepasses,
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	goober, automated := fx.goober, fx.automated
	if goober == nil {
		goober = exec
	}
	if automated == nil {
		automated = gate.NewAutomatedEvaluator()
	}
	env.RegisterActivity(&Activities{
		Journal:    writer,
		Goober:     goober,
		Det:        exec,
		Auto:       &flakyAutomated{inner: automated, transientFailures: fx.gateTransientFailures},
		Workspaces: testWorkspaces(t),
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); (err != nil) != fx.wantEngineErr {
		t.Fatalf("engine workflow error = %v, wantEngineErr = %t", err, fx.wantEngineErr)
	}
	return readJournalEvents(t, filepath.Join(runsDir, runID))
}

func readJournalEvents(t *testing.T, dir string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead %s: %v", dir, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events %s: %v", dir, err)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("journal %s violates seq monotonicity: %v", dir, err)
	}
	return events
}

// diffConformanceViews reduces both journals through journal.ConformanceView
// and compares the seq-ordered normative sequences. On divergence it names the
// first divergent position (with each side's original journal seq) and prints
// both events — debuggability is a feature (#637).
func diffConformanceViews(local, engine []journal.Event) error {
	lv, ev := journal.ConformanceView(local), journal.ConformanceView(engine)
	limit := len(lv)
	if len(ev) < limit {
		limit = len(ev)
	}
	for i := 0; i < limit; i++ {
		if lv[i] != ev[i] {
			return fmt.Errorf(
				"conformance diverges at normative event %d (local seq %d, engine seq %d):\n  local:  %s\n  engine: %s",
				i+1, normativeSeqAt(local, i), normativeSeqAt(engine, i), lv[i], ev[i])
		}
	}
	if len(lv) != len(ev) {
		longerName, longer, longerEvents := "engine", ev, engine
		if len(lv) > len(ev) {
			longerName, longer, longerEvents = "local", lv, local
		}
		return fmt.Errorf(
			"conformance diverges at normative event %d: %s journal has %d extra event(s), first extra (seq %d):\n  %s",
			limit+1, longerName, len(longer)-limit, normativeSeqAt(longerEvents, limit), longer[limit])
	}
	return nil
}

// normativeSeqAt maps a normative-view index back to the original event's seq,
// using the same IsConformanceNormative membership the shared view applies.
func normativeSeqAt(events []journal.Event, idx int) uint64 {
	n := 0
	for _, e := range events {
		if !e.IsConformanceNormative() {
			continue
		}
		if n == idx {
			return e.Seq
		}
		n++
	}
	return 0
}

// TestConformanceDualRunnerJournalParity is the dual-runner harness: every
// fixture runs through both runners; the live engine journal must be
// indistinguishable from the local runner's on the conformance surface.
func TestConformanceDualRunnerJournalParity(t *testing.T) {
	for i, fx := range conformanceFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			runID := fmt.Sprintf("conf-%02d", i)
			local := runLocalFixture(t, fx, runID)
			engine := runEngineFixture(t, fx, runID)

			// Guard the comparison itself: an accidentally empty view would
			// make every fixture vacuously "conformant".
			view := journal.ConformanceView(local)
			if len(view) < 3 {
				t.Fatalf("local conformance view has only %d events — the fixture proves nothing", len(view))
			}
			if view[0].Type != journal.EventRunStarted || view[len(view)-1].Type != journal.EventRunFinished {
				t.Fatalf("local view does not span run.started..run.finished: first=%s last=%s", view[0].Type, view[len(view)-1].Type)
			}
			if fx.usesRepo && !viewContains(view, journal.EventRefTouched) {
				t.Fatalf("repo-using fixture's view carries no ref.touched event")
			}
			if len(fx.spec.Gates) > 0 && !viewContains(view, journal.EventGateEvaluated) {
				t.Fatalf("gated fixture's view carries no gate.evaluated event")
			}

			if err := diffConformanceViews(local, engine); err != nil {
				t.Fatalf("fixture %q: %v", fx.name, err)
			}
		})
	}
}

func viewContains(view []journal.NormativeEvent, typ journal.EventType) bool {
	for _, e := range view {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// TestConformanceDiffNamesFirstDivergence is the intentionally-divergent
// self-test #637 requires: the harness's failure output must name the first
// divergent position and print both events.
func TestConformanceDiffNamesFirstDivergence(t *testing.T) {
	base := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted, Status: "running"},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Seq: 3, Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: 1}, // excluded
		{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
		{Seq: 5, Type: journal.EventRunFinished, Status: "completed"},
	}
	divergent := make([]journal.Event, len(base))
	copy(divergent, base)
	divergent[3].Status = "failure"

	if err := diffConformanceViews(base, base); err != nil {
		t.Fatalf("identical journals must not diverge: %v", err)
	}

	err := diffConformanceViews(base, divergent)
	if err == nil {
		t.Fatal("divergent journals reported as conformant")
	}
	msg := err.Error()
	// The divergent event is normative position 3 (the heartbeat at seq 3 is
	// excluded by the shared view), original seq 4 on both sides.
	for _, want := range []string{"normative event 3", "local seq 4", "engine seq 4", "status=success", "status=failure"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diff output missing %q:\n%s", want, msg)
		}
	}

	// A missing tail is reported with the extra event, not silently accepted.
	err = diffConformanceViews(base, base[:len(base)-1])
	if err == nil {
		t.Fatal("truncated journal reported as conformant")
	}
	if !strings.Contains(err.Error(), "extra event") || !strings.Contains(err.Error(), "run.finished") {
		t.Errorf("truncation diff output unhelpful: %v", err)
	}
}
