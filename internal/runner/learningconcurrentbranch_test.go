package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// #3932. The local runner has two walks that reach a gate's retry arm:
// stepGate, which serves the sequential walk AND every branch of a parallel
// executed one-at-a-time, and runBranch, which serves the concurrent walk when
// maxConcurrentBranches > 1. The second had a hand-copied HALF of the first —
// the reviewer's verdict pointer and not the learning episode — so a repass
// inside a concurrent branch was journaled, dispatched and graded without ever
// receiving the correction that a byte-identical definition produced when the
// same parallel happened to be executed sequentially.
//
// maxConcurrentBranches is a scheduling bound. It bounds worktree and process
// concurrency; it is tuned for machine capacity, and it is routinely different
// between a developer's laptop, CI and a deployment. It is not a semantic
// switch, and nothing in the DSL says it is. So the load-bearing statement is
// EQUIVALENCE, not "the concurrent walker also injects": the same definition
// and the same failures must produce the same corrections at width 1 and at
// width 2.
//
// The fixture is a two-branch continue_on_error parallel where branch a's gate
// sends work BACK to the stage inside the branch that produced the failure —
// the true-repass shape #3929's ruling admits:
//
//	fan ─┬─ a: impl-a ──> gate-a {pass: @join, fail: impl-a}
//	     └─ b: lens-b ──> @join
//	                       └──> collate
func TestConcurrentAndSequentialBranchesInjectTheSameCorrection(t *testing.T) {
	widths := []int{1, 2}
	byWidth := map[int][]learningInjectionRecord{}
	for _, width := range widths {
		width := width
		t.Run(fmt.Sprintf("maxConcurrentBranches=%d", width), func(t *testing.T) {
			run := runBranchRepassFixture(t, fmt.Sprintf("run-branch-repass-%d", width),
				branchRepassMachine(t, width, "impl-a"))

			// The branch really did repass: impl-a ran twice INSIDE branch 1.
			if got := countStageStarts(run.events, "impl-a"); got != 2 {
				t.Fatalf("impl-a started %d time(s), want 2 — the fixture must actually take the "+
					"send-back for there to be a correction to inject", got)
			}
			if len(run.injections) != 1 {
				t.Fatalf("learning injections = %d, want exactly 1:\n%s",
					len(run.injections), formatInjections(run.injections))
			}
			injection := run.injections[0]
			want := learningInjectionRecord{
				Gate: "gate-a", Subject: "impl-a", Target: "impl-a",
				AnnotationStage: "impl-a", AnnotationAttempt: 2,
				SourceAttempt: 1, NextAttempt: 2,
			}
			if injection.comparable() != want {
				t.Fatalf("injection = %+v, want %+v", injection.comparable(), want)
			}

			// Branch-scoped, at BOTH widths. The concurrent walker journals
			// through a branchJournal that stamps the branch onto every event;
			// the sequential one routes through ws.parallel. An episode that
			// landed on branch 0 would be a run-level artifact a sibling
			// branch and the join could both read.
			if injection.Branch != 1 {
				t.Fatalf("episode annotation landed on branch %d, want branch 1 (a) — the correction "+
					"belongs to the branch that produced the failure, not to the run", injection.Branch)
			}

			// The pointer reached the DISPATCH it was addressed to. This is
			// the difference between journaling a correction and delivering
			// one, and it is the whole of what #3932 restores for the
			// concurrent walker.
			second := run.capture.envelopes("impl-a")
			if len(second) != 2 {
				t.Fatalf("impl-a was dispatched %d time(s), want 2", len(second))
			}
			// The pointer NAME is keyed on the corrected event's journal
			// sequence, and sequence numbers legitimately differ between the
			// two widths because a concurrent parallel interleaves its
			// branches into one journal. So the cross-width claim below is
			// about the arithmetic and the placement, not the name; the name
			// is pinned here instead, against the event it must address.
			corrected := failedStageSeq(t, run.events, "impl-a")
			if want := LearningEpisodePointerName(corrected); injection.PointerName != want {
				t.Fatalf("episode pointer = %q, want %q — the pointer names the corrected event, and "+
					"the repass resolves the correction by that name", injection.PointerName, want)
			}
			if !envelopeCarriesEpisode(second[1], injection.PointerName) {
				t.Fatalf("the repass of impl-a carried pointers %v, want the episode pointer %q",
					envelopePointerNames(second[1]), injection.PointerName)
			}

			// The correction is fabricated context, so the work graded on it
			// must say so. The downgrade is what stops a later reader treating
			// a repass's output as independent evidence.
			if !stageFinishedDerived(run.events, "impl-a") {
				t.Fatal("no impl-a stage.finished carries derived integrity — a stage graded on an " +
					"injected correction must be downgraded, at either width")
			}

			// Branch accounting. The episode is a recorded artifact and a
			// produced output, so a branch that injected one must not settle
			// "no-output" — the completeness record a join switches on would
			// otherwise disagree between the two widths.
			if got, want := flattenCompleteness(run.completeness), "1:a:succeeded;2:b:no-output"; got != want {
				t.Fatalf("completeness = %q, want %q — the injected episode is a real artifact and a "+
					"real produced output, and the join reads this record", got, want)
			}
			for _, e := range run.events {
				if e.Type != journal.EventBranchFinished {
					continue
				}
				switch e.BranchStatus {
				case journal.BranchFailed, journal.BranchCancelled, journal.BranchTimedOut:
					t.Fatalf("branch %q finished %q — sharing the arm must not change how a branch settles",
						e.BranchName, e.BranchStatus)
				}
			}
			byWidth[width] = run.injections
		})
	}

	// The equivalence itself, stated over the whole projection rather than
	// over a count: two walkers that inject "an episode each" but disagree on
	// its gate, subject, target, attempt arithmetic or annotation placement
	// are still drifting.
	if t.Failed() {
		return
	}
	sequential, concurrent := byWidth[1], byWidth[2]
	if len(sequential) != len(concurrent) {
		t.Fatalf("width 1 injected %d correction(s), width 2 injected %d — maxConcurrentBranches is a "+
			"scheduling bound and must not decide whether a repass is corrected",
			len(sequential), len(concurrent))
	}
	for i := range sequential {
		if sequential[i].comparable() != concurrent[i].comparable() {
			t.Fatalf("injection %d differs between widths:\n  maxConcurrentBranches=1: %+v\n  "+
				"maxConcurrentBranches=2: %+v", i+1, sequential[i].comparable(), concurrent[i].comparable())
		}
		if sequential[i].Branch != concurrent[i].Branch {
			t.Fatalf("injection %d landed on branch %d sequentially and %d concurrently",
				i+1, sequential[i].Branch, concurrent[i].Branch)
		}
	}
}

// The other half of #3929's ruling, now that the concurrent walker takes the
// arm at all: a gate inside a concurrently-executed branch that routes ONWARD
// is not a repass and must inject nothing.
//
// This matters more for runBranch than for stepGate. Sharing the producer means
// runBranch newly reaches the injection code path on EVERY taken retry arm, so
// the predicate is the only thing standing between a forward branch and a
// fabricated correction. If the shared helper had used "did this gate take a
// retry arm" instead of LearningEpisodeAppliesToRepass, this is the test that
// would catch it — and it would have caught a regression that #3929 already
// paid for on the sequential side.
func TestConcurrentForwardBranchInjectsNothing(t *testing.T) {
	run := runBranchRepassFixture(t, "run-branch-forward", branchRepassMachine(t, 2, "park-a"))

	if !stageStarted(run.events, "park-a") {
		t.Fatal("park-a never started — the forward branch must still be taken")
	}
	if !stageStarted(run.events, "collate") {
		t.Fatal("collate never started — branch a must settle and reach the join")
	}
	if len(run.injections) != 0 {
		t.Fatalf("learning injections = %d, want 0 — gate-a routed ONWARD to a stage that has never "+
			"run, so there is nothing it produced to correct:\n%s",
			len(run.injections), formatInjections(run.injections))
	}

	// The retry decision itself is unconditional and survives, carrying the
	// repassAttempt 0 that is the evidence the ruling reads. Only the episode
	// is gated.
	decisions := forwardRetryDecisions(run.events)
	if len(decisions) != 1 {
		t.Fatalf("retry decision annotations = %d (%+v), want exactly 1", len(decisions), decisions)
	}
	want := runnerRetryDecision{
		Stage: "impl-a", Gate: "gate-a",
		FailureClass:  string(journal.AttemptPolicy),
		FailureCode:   "nonzero_exit",
		RepassAttempt: 0, Target: "park-a",
	}
	if decisions[0] != want {
		t.Fatalf("retry decision = %+v, want %+v", decisions[0], want)
	}

	// And the pointers the forward target IS handed are the same set the
	// sequential walker hands it. Routing both walkers' unconditional half —
	// the reviewer verdict pointer, which predates #3929 and rides every taken
	// retry arm — through one producer must not have made it conditional, or
	// reordered it, or dropped it on one side only. gate-a is automated, so the
	// set is legitimately empty here; the claim under test is that it is the
	// SAME set, which is exactly the property that broke for the episode half.
	sequentialRun := runBranchRepassFixture(t, "run-branch-forward-seq",
		branchRepassMachine(t, 1, "park-a"))
	concurrentNames := forwardTargetPointerNames(t, run, "park-a")
	sequentialNames := forwardTargetPointerNames(t, sequentialRun, "park-a")
	if !equalStrings(concurrentNames, sequentialNames) {
		t.Fatalf("park-a was handed %v concurrently and %v sequentially; the unconditional half of the "+
			"arm must not depend on which walker took it", concurrentNames, sequentialNames)
	}
	for _, name := range concurrentNames {
		if strings.HasPrefix(name, "learning.episode[") {
			t.Fatalf("a forward branch's target was handed an episode pointer %q", name)
		}
	}
	if n := len(sequentialRun.injections); n != 0 {
		t.Fatalf("the sequential control injected %d correction(s) on a forward branch, want 0", n)
	}
}

// failedStageSeq returns the journal sequence of the stage.finished that
// recorded stage's failure — the event a correction is addressed to.
func failedStageSeq(t *testing.T, events []journal.Event, stage string) uint64 {
	t.Helper()
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage == stage &&
			e.Status == string(apiv1.ResultFailure) {
			return e.Seq
		}
	}
	t.Fatalf("no failing stage.finished for %q", stage)
	return 0
}

// forwardTargetPointerNames returns, sorted, the pointer names the single
// dispatch of stage was handed.
func forwardTargetPointerNames(t *testing.T, run branchRepassFixtureRun, stage string) []string {
	t.Helper()
	envelopes := run.capture.envelopes(stage)
	if len(envelopes) != 1 {
		t.Fatalf("%s was dispatched %d time(s), want 1", stage, len(envelopes))
	}
	names := envelopePointerNames(envelopes[0])
	sort.Strings(names)
	return names
}

// The resume half of #3932, unit-stated over pendingParallel.
//
// runBranch is the only walker with a replay boundary INSIDE a branch: a branch
// resuming across a gate.evaluated re-derives the gate result from history
// rather than re-evaluating it, and passes replayed=true so the shared producer
// records nothing. That guard is only half correct on its own. The branch's
// previously recorded pointers have to come BACK, or the resumed repass
// re-enters its stage with the episode still on disk and nothing pointing at
// it: the correction is journaled and never dispatched, and the derived
// downgrade it justified silently disappears.
//
// reconstructPointers had always rebuilt the run-level episode pointer.
// pendingParallel rebuilt the verdict pointer beside it and not the episode —
// the same divergence-by-duplication as the retry arm itself, one layer down.
func TestResumedBranchKeepsItsInjectedCorrection(t *testing.T) {
	machine := branchRepassMachine(t, 2, "impl-a")
	ref := &journal.Ref{
		Path: "learning/episode-gate-a-4.json", Digest: "sha256:beef", Size: 128,
		Integrity: apiv1.IntegrityDerived,
	}
	events := []journal.Event{
		{Seq: 1, Type: journal.EventParallelStarted, Parallel: "fan"},
		{Seq: 2, Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "a", Stage: "impl-a"},
		{Seq: 3, Type: journal.EventStageStarted, Parallel: "fan", Branch: 1, Stage: "impl-a", Attempt: 1},
		{Seq: 4, Type: journal.EventStageFinished, Parallel: "fan", Branch: 1, Stage: "impl-a", Attempt: 1,
			Status: string(apiv1.ResultFailure)},
		{Seq: 5, Type: journal.EventGateEvaluated, Parallel: "fan", Branch: 1, Gate: "gate-a", Target: "impl-a",
			Ref: &journal.Ref{Path: "gates/gate-a-1.json", Digest: "sha256:cafe", Size: 64,
				Integrity: apiv1.IntegrityDerived},
			Runner: map[string]any{"repassAttempt": float64(1)}},
		{Seq: 6, Type: journal.EventRunnerAnnotation, Parallel: "fan", Branch: 1, Stage: "impl-a", Attempt: 2,
			Gate: "gate-a", Name: "learning/episode-gate-a-4.json", Ref: ref,
			Runner: map[string]any{
				"kind": LearningEpisodeInjectedKind, "target": "impl-a",
				"sourceSeq": float64(4), "nextAttempt": float64(2),
			}},
	}

	par, _ := pendingParallel(events, machine)
	if par == nil {
		t.Fatal("pending parallel is nil — the interrupted branch was not reconstructed at all")
	}
	branch := par.branch("a")
	if branch == nil {
		t.Fatal("reconstructed parallel has no branch a")
	}

	names := make([]string, 0, len(branch.pointers))
	for _, pointer := range branch.pointers {
		names = append(names, pointer.Name)
	}
	sort.Strings(names)
	if want := []string{"gate-a.verdict", LearningEpisodePointerName(4)}; !equalStrings(names, want) {
		t.Fatalf("resumed branch pointers = %v, want %v — a branch that injected a correction before "+
			"the interruption must get it back, exactly as reconstructPointers rebuilds the run-level "+
			"one; otherwise the resumed repass runs uncorrected while the artifact sits on disk",
			names, want)
	}

	var episode *apiv1.ContextPointer
	for i := range branch.pointers {
		if branch.pointers[i].Name == LearningEpisodePointerName(4) {
			episode = &branch.pointers[i]
		}
	}
	if episode == nil {
		t.Fatal("no episode pointer after the sorted-name check passed")
	}
	if episode.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("restored episode pointer integrity = %q, want %q — a fabricated correction is "+
			"derived, and the downgrade of anything graded on it follows from the pointer",
			episode.Integrity, apiv1.IntegrityDerived)
	}
	if episode.Artifact == nil {
		t.Fatal("restored episode pointer carries no artifact; the repass would have a name and no bytes")
	}
	if episode.Artifact.Digest != ref.Digest || episode.Artifact.Path != ref.Path {
		t.Fatalf("restored episode artifact = %+v, want the recorded ref %+v", episode.Artifact, ref)
	}

	// And the producer's other half: on the replay pass the shared helper must
	// record NOTHING, so the pointer restored above is not joined by a second
	// artifact and a second annotation for one injection. A nil journal is the
	// strongest way to say it — the helper must not reach the journal at all.
	out, err := recordGateBranchInjection(nil, StartInput{RunID: "run-x"}, "gate-a", "impl-a",
		gate.Result{Attempt: 1, VerdictArtifact: &apiv1.ArtifactPointer{Path: "gates/gate-a-1.json"}},
		"impl-a", apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, true)
	if err != nil {
		t.Fatalf("recordGateBranchInjection on the replay pass: %v", err)
	}
	if len(out.pointers()) != 0 {
		t.Fatalf("the replay pass produced pointers %+v, want none — pendingParallel has already "+
			"rebuilt them from history, and recording again would double-count the artifact and file "+
			"a second annotation for one injection", out.pointers())
	}
}

// --- fixtures and readers ---

// branchRepassFixtureRun is one execution of the branch fixture, reduced to
// what every case here asserts on.
type branchRepassFixtureRun struct {
	events       []journal.Event
	injections   []learningInjectionRecord
	completeness []journal.BranchOutcome
	capture      *envelopeCapture
}

func runBranchRepassFixture(t *testing.T, runID string, machine *workflow.Machine) branchRepassFixtureRun {
	t.Helper()
	capture := newEnvelopeCapture()
	script := newScriptedDeterministic(map[string][]stubTaskResult{
		runID + ":impl-a": {
			{status: apiv1.ResultFailure, summary: "the branch's work did not hold",
				errorInfo: &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "exit status 1", Retryable: true}},
			{status: apiv1.ResultSuccess},
		},
		runID + ":park-a":  {{status: apiv1.ResultSuccess}},
		runID + ":lens-b":  {{status: apiv1.ResultSuccess}},
		runID + ":collate": {{status: apiv1.ResultSuccess}},
	}, capture)

	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return script, nil
	}, gate.NewAutomatedEvaluator())
	r.cfg.ScratchDir = t.TempDir()

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "demo",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	runDir := filepath.Join(runsDir, runID)
	events := readJournalEvents(t, runDir)
	var completeness []journal.BranchOutcome
	for i := range events {
		if events[i].Type == journal.EventParallelFinished {
			completeness = events[i].Completeness
		}
	}
	if completeness == nil {
		t.Fatal("no parallel.finished event — the parallel never settled")
	}
	return branchRepassFixtureRun{
		events:       events,
		injections:   learningInjectionRecords(t, runDir, events),
		completeness: completeness,
		capture:      capture,
	}
}

// branchRepassMachine is a two-branch continue_on_error parallel whose first
// branch carries a status-equals gate. failTarget selects the arm under test:
// impl-a is the true-repass shape, park-a the forward one. width is written
// straight into MaxConcurrentBranches, which is the axis the equivalence case
// varies and the only thing that decides which walker takes the arm.
func branchRepassMachine(t *testing.T, width int, failTarget string) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	tasks := []apiv1.Task{
		task("impl-a", "gate-a"),
		task("lens-b", workflow.TargetJoin),
		task("collate", workflow.TerminalComplete),
	}
	if failTarget != "impl-a" {
		tasks = append(tasks, task(failTarget, workflow.TargetJoin))
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "branch-repass", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks:    tasks,
			Gates: []apiv1.Gate{{
				Name:      "gate-a",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TargetJoin, "fail": failTarget},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				MaxConcurrentBranches: int32(width),
				Join:                  "collate",
				Branches: []apiv1.Branch{
					{Name: "a", Start: "impl-a"},
					{Name: "b", Start: "lens-b"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile branch-repass fixture: %v", err)
	}
	return machine
}

// stageFinishedDerived reports whether any stage.finished for stage carries
// derived integrity — the downgrade that says the work was graded on
// fabricated context.
func stageFinishedDerived(events []journal.Event, stage string) bool {
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage == stage &&
			e.Integrity == apiv1.IntegrityDerived {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConcurrentBranchInjectsOnTheAdvancePathToo is the same equivalence claim
// on the arm #3943 opened, and it is the reason this file could not simply be
// merged with that change and left alone.
//
// #3943 established that a stage-re-entering branch reaches its target by
// EITHER route: an agentic reviewer's needs-changes is the canonical true
// repass of the system, and it is precisely the branch the retry classifier
// declines, so routeRetryDecision returns retry == false and the walk takes
// the ADVANCE path. #3943 wired an injection into walk()'s advance path, which
// is the sequential walker's. runBranch has its OWN advance path, so wiring
// only the retry arm would have rebuilt #3932's exact divergence — this time
// for the branch that matters most, and only at maxConcurrentBranches > 1.
//
// Both routes now go through one producer, so the assertion is again
// equivalence across the scheduling bound rather than "the concurrent walker
// also injects".
func TestConcurrentBranchInjectsOnTheAdvancePathToo(t *testing.T) {
	byWidth := map[int]learningInjectionRecord{}
	for _, width := range []int{1, 2} {
		t.Run(fmt.Sprintf("maxConcurrentBranches=%d", width), func(t *testing.T) {
			run := runAgenticBranchRepassFixture(t, fmt.Sprintf("run-branch-agentic-%d", width), width)

			// The branch really did repass, and it did so WITHOUT a retry
			// decision: that is what makes this the advance path rather than
			// the retry arm the sibling case covers.
			if got := countStageStarts(run.events, "impl-a"); got != 2 {
				t.Fatalf("impl-a started %d time(s), want 2 — the reviewer's needs-changes must "+
					"actually send the branch back for there to be a correction to inject", got)
			}
			if got := forwardRetryDecisions(run.events); len(got) != 0 {
				t.Fatalf("retry decision annotations = %d (%+v), want 0 — a reviewer verdict is not a "+
					"retry-classifiable failure, so this branch must travel the advance path", len(got), got)
			}
			if len(run.injections) != 1 {
				t.Fatalf("learning injections = %d, want exactly 1:\n%s",
					len(run.injections), formatInjections(run.injections))
			}
			injection := run.injections[0]
			want := learningInjectionRecord{
				Gate: "review-a", Subject: "impl-a", Target: "impl-a",
				AnnotationStage: "impl-a", AnnotationAttempt: 2,
				SourceAttempt: 1, NextAttempt: 2,
			}
			if injection.comparable() != want {
				t.Fatalf("injection = %+v, want %+v", injection.comparable(), want)
			}
			if injection.Branch != 1 {
				t.Fatalf("episode annotation landed on branch %d, want branch 1 (a)", injection.Branch)
			}

			// Delivered, not merely journaled.
			second := run.capture.envelopes("impl-a")
			if len(second) != 2 {
				t.Fatalf("impl-a was dispatched %d time(s), want 2", len(second))
			}
			// An agentic gate addresses the episode by the REVIEWER's event —
			// the gate.evaluated that carries the verdict — not by a failing
			// stage.finished, because the stage did not fail: the reviewer
			// rejected its output.
			corrected := needsChangesGateSeq(t, run.events, "review-a")
			if want := LearningEpisodePointerName(corrected); injection.PointerName != want {
				t.Fatalf("episode pointer = %q, want %q — the pointer names the reviewer's verdict "+
					"event, and the repass resolves the correction by that name", injection.PointerName, want)
			}
			if !envelopeCarriesEpisode(second[1], injection.PointerName) {
				t.Fatalf("the repass of impl-a carried pointers %v, want the episode pointer %q",
					envelopePointerNames(second[1]), injection.PointerName)
			}
			if !stageFinishedDerived(run.events, "impl-a") {
				t.Fatal("no impl-a stage.finished carries derived integrity — a stage graded on an " +
					"injected correction must be downgraded on the advance path too")
			}
			if got, want := flattenCompleteness(run.completeness), "1:a:succeeded;2:b:no-output"; got != want {
				t.Fatalf("completeness = %q, want %q", got, want)
			}
			byWidth[width] = injection.comparable()
		})
	}
	if byWidth[1] != byWidth[2] {
		t.Fatalf("the two widths produced different corrections on the advance path:\n"+
			"  width 1: %+v\n  width 2: %+v\n"+
			"maxConcurrentBranches is a scheduling bound, not a semantic one", byWidth[1], byWidth[2])
	}
}

// runAgenticBranchRepassFixture is runBranchRepassFixture's agentic twin: the
// branch's gate is an agentic reviewer that returns needs-changes and then
// pass, so the send-back is not retry-classifiable and the branch walker must
// reach the injection through its advance path.
func runAgenticBranchRepassFixture(t *testing.T, runID string, width int) branchRepassFixtureRun {
	t.Helper()
	capture := newEnvelopeCapture()
	script := newScriptedDeterministic(map[string][]stubTaskResult{
		runID + ":impl-a":  {{status: apiv1.ResultSuccess}, {status: apiv1.ResultSuccess}},
		runID + ":lens-b":  {{status: apiv1.ResultSuccess}},
		runID + ":collate": {{status: apiv1.ResultSuccess}},
	}, capture)
	reviewer := &scriptedReviewer{t: t, verdicts: []apiv1.Verdict{
		{Decision: apiv1.VerdictNeedsChanges, Summary: "the branch's work does not hold",
			Rationale: "handle the empty case before the scan rather than after it"},
		{Decision: apiv1.VerdictPass, Summary: "the empty case is handled now"},
	}}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return script, nil },
		NewAgentic:       func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return reviewer, nil },
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.cfg.ScratchDir = t.TempDir()

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: agenticBranchRepassMachine(t, width),
		Gaggle:  "demo",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	runDir := filepath.Join(runsDir, runID)
	events := readJournalEvents(t, runDir)
	var completeness []journal.BranchOutcome
	for i := range events {
		if events[i].Type == journal.EventParallelFinished {
			completeness = events[i].Completeness
		}
	}
	if completeness == nil {
		t.Fatal("no parallel.finished event — the parallel never settled")
	}
	return branchRepassFixtureRun{
		events:       events,
		injections:   learningInjectionRecords(t, runDir, events),
		completeness: completeness,
		capture:      capture,
	}
}

func agenticBranchRepassMachine(t *testing.T, width int) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "branch-agentic-repass", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("impl-a", "review-a"),
				task("lens-b", workflow.TargetJoin),
				task("collate", workflow.TerminalComplete),
			},
			Gates: []apiv1.Gate{{
				Name:      "review-a",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceRepoReadOnly},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         workflow.TargetJoin,
					string(apiv1.VerdictNeedsChanges): "impl-a",
					gate.OutcomeFail:                  workflow.TargetJoin,
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				MaxConcurrentBranches: int32(width),
				Join:                  "collate",
				Branches: []apiv1.Branch{
					{Name: "a", Start: "impl-a"},
					{Name: "b", Start: "lens-b"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile branch-agentic-repass fixture: %v", err)
	}
	return machine
}

// needsChangesGateSeq returns the sequence of the first gate.evaluated event
// for gateName that sent work back — the event an agentic episode is addressed
// to.
func needsChangesGateSeq(t *testing.T, events []journal.Event, gateName string) uint64 {
	t.Helper()
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated && e.Gate == gateName && e.Runner != nil {
			if attempt, _ := runnerInt(e.Runner["repassAttempt"]); attempt >= 1 {
				return e.Seq
			}
		}
	}
	t.Fatalf("no repassing gate.evaluated for %q", gateName)
	return 0
}
