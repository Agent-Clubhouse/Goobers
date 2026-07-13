// This file is the V0 walking skeleton (issue #29): it drives a single
// backlog item through the REAL local runner (internal/runner, #17) — real
// journal on disk, real git worktrees, a fake harness (#19's FakeAdapter)
// standing in for the Copilot CLI, and no network access. It replaces the
// earlier version of this file, which proved the same state machine only
// inside Temporal's in-memory test environment — that coverage of the V2
// Temporal adapter now lives in internal/engine's own test suite
// (internal/engine/*_test.go); this file is V0's standing integration gate
// and the seed of the V2 local↔Temporal conformance harness
// (docs/ARCHITECTURE.md §3.3). test/e2e/integration_test.go is untouched: it
// exercises the separate, still-compiling quarantined tier-3 wired path
// (bootstrap/scheduler/engine) against its own fixture.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// --- fixture repo: a local bare git repo, so the walking skeleton needs no
// network access (issue #29 acceptance: "green in CI on a runner with no
// network access"). ---

func newSkeletonFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runSkeletonGit(t, work, "init", "--initial-branch=main")
	runSkeletonGit(t, work, "config", "user.email", "test@example.com")
	runSkeletonGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSkeletonGit(t, work, "add", "README.md")
	runSkeletonGit(t, work, "commit", "-m", "initial")
	runSkeletonGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runSkeletonGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

// --- fixture workflow: multi-stage with a gate repass (issue #29 scope):
// implement (agentic) -> review (agentic gate; needs-changes repasses to
// implement once, then passes) -> local-ci (deterministic) -> terminal. ---

func skeletonMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement the backlog item",
				// Matches config-examples/gaggles/acme-web/workflows/implementation.yaml's
				// real "implement" task (#27) — a modest retry budget beyond
				// the crash boundary a mid-attempt kill consumes (see
				// TestWalkingSkeletonCrashResume). MaxAttempts=1 (the
				// no-Retry default) would make ANY mid-attempt crash
				// unresumable by design (internal/runner/resume.go
				// fail-closed contract) — a real agentic task budgets for
				// this on purpose.
				Retry: &apiv1.RetryPolicy{MaxAttempts: 2},
				Next:  "review",
			},
			{Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run the local CI-equivalent", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          "local-ci",
					"needs-changes": "implement",
					"fail":          workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "walking-skeleton", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile skeleton machine: %v", err)
	}
	return m
}

// --- the runner wiring: real ShellExecutor (#18) for deterministic tasks,
// real harness.Executor (#19) per goober for agentic tasks/gates, real
// gate.Evaluator (#20) for gates, real worktree.Manager (#16) and journal
// (#8) — only the goober harness's Copilot subprocess is faked. ---

func newSkeletonRunner(t *testing.T, coderAct, reviewerAct func(callNum int) interface{}) (*runner.Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newSkeletonFixtureRepo(t)

	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}

	r, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, ierr := credentials.NewInjector(resolver, nil, reg)
			if ierr != nil {
				return nil, ierr
			}
			return executor.NewShellExecutor(injector, rec)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			act := coderAct
			if gooberName == "reviewer" {
				act = reviewerAct
			}
			injector, ierr := credentials.NewInjector(resolver, nil, reg)
			if ierr != nil {
				return nil, ierr
			}
			calls := 0
			adapter := &harness.FakeAdapter{
				Transcript: []byte("fake harness session transcript for " + gooberName + "\n"),
				Act: func(_ context.Context, req harness.RunRequest) error {
					calls++
					return harness.WriteCompletion(req.Workspace, req.CompletionPath, act(calls))
				},
			}
			// The runner always constructs executors against the run's own
			// *journal.Run, which implements harness.SpanRecorder alongside
			// runner.ArtifactRecorder (same RecordSpan method) — asserting
			// that here avoids widening runner.ArtifactRecorder itself.
			recorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement harness.SpanRecorder", rec)
			}
			// reg is this run's own *journal.RegistryScrubber (#66) — it also
			// implements journal.Scrubber, so chaining it with the pattern
			// net gives the harness executor the SAME per-run scrubbing the
			// runner's own journal uses, rather than a disconnected one.
			registryScrubber, ok := reg.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("test double %T does not implement journal.Scrubber", reg)
			}
			scrubber := journal.Chain(registryScrubber, journal.NewPatternScrubber())
			// rec is this run's own *journal.Run, which also satisfies
			// harness.ArtifactRecorder structurally (same RecordArtifact
			// method as runner.ArtifactRecorder) — passed straight through,
			// same as recorder above.
			return harness.NewExecutor(adapter, injector, recorder, rec, scrubber, "you are the "+gooberName+" fixture goober")
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, runsDir
}

func resultPayload(status apiv1.ResultStatus, summary string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  status,
		Summary: summary,
		// Outputs are scalar-only by contract (api/v1alpha1.ResultEnvelope,
		// #10) — a changed-files artifact would be a real ArtifactPointer in
		// a real run; this fixture only needs a small scalar to prove the
		// envelope round-trips.
		Outputs: map[string]interface{}{"changedFileCount": 1},
	}
}

func verdictPayload(decision apiv1.VerdictDecision, rationale string) apiv1.Verdict {
	return apiv1.Verdict{Decision: decision, Rationale: rationale}
}

// skeletonStartInput builds the StartInput for one run of the skeleton
// machine, claiming a single fixture backlog item.
func skeletonStartInput(runID string, machine *workflow.Machine) runner.StartInput {
	return runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerItem},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item: &apiv1.BacklogItem{
			ID: "101", Provider: apiv1.ProviderGitHub,
			Title: "Add walking skeleton smoke path", Labels: []string{"goobers:ready"},
			URL: "https://github.com/acme/web/issues/101",
		},
	}
}

// TestWalkingSkeletonLocalRunnerCompletesWithRepass is the headline walking
// skeleton (issue #29): a single item runs through the real local runner
// across a multi-stage workflow with a gate repass (reviewer requests
// changes once, then approves), asserting on the JOURNAL — not on runner
// internals — per the acceptance criteria: event sequence, digests verify,
// state.json terminal, artifacts resolvable, spans present.
func TestWalkingSkeletonLocalRunnerCompletesWithRepass(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(call int) interface{} {
		if call == 1 {
			return verdictPayload(apiv1.VerdictNeedsChanges, "add a test for the new branch")
		}
		return verdictPayload(apiv1.VerdictPass, "looks good")
	}
	r, runsDir := newSkeletonRunner(t, coderAct, reviewerAct)

	res, err := r.Start(context.Background(), skeletonStartInput("run-skeleton-1", machine))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if res.FinalState != "local-ci" {
		t.Fatalf("finalState = %q, want local-ci", res.FinalState)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-skeleton-1"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}

	// Identity: version-pinned (WF-016) to the exact compiled digest.
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.WorkflowDigest != machine.Digest() {
		t.Errorf("run.yaml workflowDigest = %q, want %q", id.WorkflowDigest, machine.Digest())
	}

	// Event sequence: implement runs, review requests changes (repass),
	// implement runs again, review passes, local-ci runs, run finishes.
	// Artifacts/spans interleave per stage but every stage/gate transition is
	// present and in order.
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var types []journal.EventType
	sawSpan, sawArtifact := false, false
	for _, e := range events {
		types = append(types, e.Type)
		if e.Type == journal.EventSpanRecorded {
			sawSpan = true
		}
		if e.Type == journal.EventArtifactRecorded {
			sawArtifact = true
		}
	}
	if !sawSpan {
		t.Error("expected at least one span.recorded event (the fake harness transcript)")
	}
	if !sawArtifact {
		t.Error("expected at least one artifact.recorded event")
	}
	gateEvals := countEventType(types, journal.EventGateEvaluated)
	if gateEvals != 2 {
		t.Errorf("gate.evaluated count = %d, want 2 (needs-changes then pass)", gateEvals)
	}
	stageStarts := countEventType(types, journal.EventStageStarted)
	if stageStarts != 3 {
		t.Errorf("stage.started count = %d, want 3 (implement x2, local-ci x1)", stageStarts)
	}
	if types[0] != journal.EventRunStarted || types[len(types)-1] != journal.EventRunFinished {
		t.Errorf("event sequence must start with run.started and end with run.finished, got %v", types)
	}

	// state.json: terminal, no pending machine state.
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted || st.MachineState != "" {
		t.Fatalf("state.json = %+v, want completed with empty machineState", st)
	}

	// Every recorded artifact/span is resolvable and digest-verified — the
	// same round-trip a downstream stage or the portal would do.
	for _, e := range events {
		switch e.Type {
		case journal.EventArtifactRecorded:
			if _, err := rd.ArtifactBytes(*e.Ref); err != nil {
				t.Errorf("ArtifactBytes(%+v): %v", e.Ref, err)
			}
		case journal.EventSpanRecorded:
			if _, err := rd.SpanBytes(*e.Ref); err != nil {
				t.Errorf("SpanBytes(%+v): %v", e.Ref, err)
			}
		}
	}
}

// TestWalkingSkeletonLocalRunnerGateFailAborts exercises the "fail" branch:
// review rejects outright and the run ends aborted without a repass.
func TestWalkingSkeletonLocalRunnerGateFailAborts(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(int) interface{} { return verdictPayload(apiv1.VerdictFail, "wrong approach entirely") }
	r, _ := newSkeletonRunner(t, coderAct, reviewerAct)

	res, err := r.Start(context.Background(), skeletonStartInput("run-skeleton-2", machine))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", res.Phase)
	}
}

// TestWalkingSkeletonLocalRunnerDeterministicJournal is the conformance seed
// (docs/ARCHITECTURE.md §3.3): two independent runs of the identical
// definition, with the SAME pinned RunID (§3.3's conformance comparison is
// apples-to-apples over identical caller-supplied inputs, not two arbitrary
// runs — RunID is caller-supplied, never runner-generated, so a real
// conformance harness pins it identically across the runners it compares)
// and a deterministic fake harness (no live LLM variance), produce
// digest-identical event sequences over the conformance-normative field set
// — timestamps, durations, infra-retry attempts, and namespaced runner.*
// annotations excluded per §3.3 and journal.Event.IsConformanceNormative/doc
// comments. (Artifact/span Name legitimately embeds the RunID via
// env.TaskID, per internal/executor.ShellExecutor — same RunID is what makes
// Name comparable at all, not something to strip from the comparison.) This
// is the seed the V2 local↔Temporal conformance harness (ARCHITECTURE §3.3,
// issue tracked for V2) extends to diff the two runners' journals against
// shared fixtures.
func TestWalkingSkeletonLocalRunnerDeterministicJournal(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(call int) interface{} {
		if call == 1 {
			return verdictPayload(apiv1.VerdictNeedsChanges, "add a test for the new branch")
		}
		return verdictPayload(apiv1.VerdictPass, "looks good")
	}

	canon := func(runID string) []string {
		r, runsDir := newSkeletonRunner(t, coderAct, reviewerAct)
		res, err := r.Start(context.Background(), skeletonStartInput(runID, machine))
		if err != nil {
			t.Fatalf("Start(%s): %v", runID, err)
		}
		if res.Phase != journal.PhaseCompleted {
			t.Fatalf("Start(%s) phase = %q, want completed", runID, res.Phase)
		}
		rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
		if err != nil {
			t.Fatalf("OpenRead(%s): %v", runID, err)
		}
		events, err := rd.Events()
		if err != nil {
			t.Fatalf("Events(%s): %v", runID, err)
		}
		return canonicalizeNormative(events)
	}

	// Same RunID for both — newSkeletonRunner gives each call its own fresh
	// instance root/runsDir, so there's no on-disk collision, and pinning the
	// identity is what makes RunID-embedding fields (artifact/span Name)
	// comparable across the two runs.
	const pinnedRunID = "run-skeleton-det"
	a := canon(pinnedRunID)
	b := canon(pinnedRunID)

	if len(a) != len(b) {
		t.Fatalf("normative event count differs: %d vs %d\na=%v\nb=%v", len(a), len(b), a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("normative event %d differs:\n a: %s\n b: %s", i, a[i], b[i])
		}
	}
}

// simulateSkeletonCrashMidImplement hand-builds a run journal exactly to the
// point a real Start would have reached had the process died right after
// dispatching "implement"'s first attempt: run.started, then
// stage.started(implement, attempt 1), with NO matching stage.finished — the
// crash signature Resume must detect (internal/runner/resume.go's
// interruptedAttempt). Mirrors internal/runner's own
// simulateCrashMidAttempt (run_test.go) exactly, but through this file's
// real StartInput shape (including the item snapshot input a genuine Start
// would have journaled) so Resume reconstructs the same run a real
// crash-mid-attempt would have left on disk. A clean journal.Create/Close
// (no torn write) is sufficient — torn-write repair is internal/journal's
// own, already-tested concern; this is about the runner's interpretation of
// "started with no finished".
func simulateSkeletonCrashMidImplement(t *testing.T, runsDir string, machine *workflow.Machine, in runner.StartInput) {
	t.Helper()
	inputs := map[string][]byte{}
	if in.Item != nil {
		b, err := json.Marshal(in.Item)
		if err != nil {
			t.Fatalf("simulateSkeletonCrashMidImplement: marshal item snapshot: %v", err)
		}
		inputs["item"] = b
	}
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: in.RunID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: in.Gaggle, Trigger: in.Trigger,
	}, inputs)
	if err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: append stage.started: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("simulateSkeletonCrashMidImplement: close: %v", err)
	}
}

// TestWalkingSkeletonCrashResume is #29's crash/resume acceptance scenario,
// now that internal/runner Deliverable B (#17) has landed: kill the runner
// mid-attempt on "implement", restart against a fresh Runner (a real
// process restart constructs a new one), and Resume from the journal's
// checkpointed state. Per resume.go's contract the interrupted attempt is
// journaled as a terminal infra-tagged failure — never silently re-run
// (§17) — before the runner continues the SAME attempt count against
// "implement"'s own retry budget, then the run rejoins the ordinary
// walking-skeleton machine (review passes, local-ci runs, completes) exactly
// as internal/runner's own TestRunnerResumeRetriesInterruptedAttempt proves
// at the runner-unit level — this test proves the same contract holds
// end-to-end through the real multi-stage/gate skeleton, asserting on the
// journal per this file's convention.
func TestWalkingSkeletonCrashResume(t *testing.T) {
	machine := skeletonMachine(t)
	coderAct := func(int) interface{} { return resultPayload(apiv1.ResultSuccess, "implemented") }
	reviewerAct := func(int) interface{} { return verdictPayload(apiv1.VerdictPass, "looks good") }
	r, runsDir := newSkeletonRunner(t, coderAct, reviewerAct)

	in := skeletonStartInput("run-skeleton-crash", machine)
	simulateSkeletonCrashMidImplement(t, runsDir, machine, in)

	res, err := r.Resume(context.Background(), runner.ResumeInput{
		RunID:   in.RunID,
		Machine: machine,
		RepoRef: in.RepoRef,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, in.RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	// "implement" saw exactly one failed attempt (the crash, journaled by
	// Resume) and one success — the acceptance scenario's own words.
	var implementEvents []journal.Event
	var types []journal.EventType
	for _, e := range events {
		types = append(types, e.Type)
		if e.Stage == "implement" {
			implementEvents = append(implementEvents, e)
		}
	}
	wantTypes := []journal.EventType{
		journal.EventStageStarted,  // attempt 1, pre-crash (hand-built above)
		journal.EventStageFinished, // attempt 1, infra, journaled by Resume
		journal.EventStageStarted,  // attempt 2, policy
		journal.EventStageFinished, // attempt 2, policy, success
	}
	if len(implementEvents) != len(wantTypes) {
		t.Fatalf("implement-stage events = %d, want %d: %+v", len(implementEvents), len(wantTypes), implementEvents)
	}
	for i, e := range implementEvents {
		if e.Type != wantTypes[i] {
			t.Errorf("event[%d].Type = %q, want %q", i, e.Type, wantTypes[i])
		}
	}
	if implementEvents[1].Attempt != 1 || implementEvents[1].AttemptClass != journal.AttemptInfra || implementEvents[1].Status != string(apiv1.ResultFailure) {
		t.Errorf("interrupted-attempt event = %+v, want attempt=1 class=infra status=failure", implementEvents[1])
	}
	// The infra-tagged interrupted attempt is excluded from conformance
	// (§3.3) — confirm IsConformanceNormative agrees, same as
	// internal/runner's own crash-resume test.
	if implementEvents[1].IsConformanceNormative() {
		t.Error("the infra-tagged interrupted attempt must be excluded from conformance (§3.3)")
	}
	if implementEvents[2].Attempt != 2 || implementEvents[2].AttemptClass != journal.AttemptPolicy {
		t.Errorf("resumed-attempt stage.started = %+v, want attempt=2 class=policy", implementEvents[2])
	}
	if implementEvents[3].Attempt != 2 || implementEvents[3].Status != string(apiv1.ResultSuccess) {
		t.Errorf("resumed-attempt stage.finished = %+v, want attempt=2 status=success", implementEvents[3])
	}

	// Resume doesn't just recover the interrupted stage in isolation — the
	// run rejoins the SAME multi-stage/gate machine the rest of #29
	// exercises: review evaluates once (pass, no repass in this scenario)
	// and local-ci runs before the run finishes completed.
	if gateEvals := countEventType(types, journal.EventGateEvaluated); gateEvals != 1 {
		t.Errorf("gate.evaluated count = %d, want 1 (single pass)", gateEvals)
	}
	if n := countEventType(types, journal.EventStageStarted); n != 3 {
		t.Errorf("stage.started count = %d, want 3 (implement x2 incl. crashed attempt, local-ci x1)", n)
	}

	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted || st.MachineState != "" {
		t.Fatalf("state.json = %+v, want completed with empty machineState", st)
	}
}

// countEventType counts occurrences of typ in types.
func countEventType(types []journal.EventType, typ journal.EventType) int {
	n := 0
	for _, t := range types {
		if t == typ {
			n++
		}
	}
	return n
}

// canonicalizeNormative projects each conformance-normative event
// (journal.Event.IsConformanceNormative) down to a stable string of only its
// normative fields, in the doc-commented normative/excluded split
// (internal/journal/event.go): Time, Ref.Path/Size, Error.Message, and the
// entire Runner map are excluded; Ref.Digest, Error.Code, and every other
// orchestration field are kept.
func canonicalizeNormative(events []journal.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if !e.IsConformanceNormative() {
			continue
		}
		digest := ""
		if e.Ref != nil {
			digest = e.Ref.Digest
		}
		errCode := ""
		if e.Error != nil {
			errCode = e.Error.Code
		}
		out = append(out, fmtNormative(e, digest, errCode))
	}
	return out
}

func fmtNormative(e journal.Event, refDigest, errCode string) string {
	return string(e.Type) + "|" + e.Stage + "|" + e.Gate + "|" + e.Verdict + "|" + e.Target + "|" +
		e.Status + "|" + e.Name + "|" + refDigest + "|" + errCode
}
