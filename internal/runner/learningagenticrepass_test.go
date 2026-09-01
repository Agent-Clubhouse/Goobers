package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// #3942: the residual of the #3929 ruling.
//
// #3929 ruled that a learning episode is injected IFF the branch is a true
// repass, and #3938 hoisted that predicate so both drivers would read it the
// same way. Neither touched the condition WRAPPING the call: the injection sat
// inside the retry-decision arm, so it also required `retryable` —
// retryFailureClassForGateResult, true only for an automated `status-equals`
// gate over nonzero_exit/base_sync_conflict, or for any gate resolving `infra`.
//
// An agentic reviewer's `needs-changes` is neither, so the canonical repass of
// the whole system — reviewer sends the implementer back — was the ONE true
// repass that never received a correction. Both drivers declined it in
// agreement, which is why no parity row was red.
//
// The predicate is now LearningEpisodeAppliesToBranch, called from both of this
// package's gate arms (stepGate's retry route and walk's advance route) and
// from internal/engine's.

// TestLearningEpisodeAppliesToBranchIsTheCanonicalPredicate is the table form
// of the ruling as #3942 extends it. Each row names the branch shape and the
// reason, because the value of this predicate is entirely in what it EXCLUDES.
func TestLearningEpisodeAppliesToBranchIsTheCanonicalPredicate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		branch LearningEpisodeBranch
		want   bool
		why    string
	}{
		{
			name:   "agentic reviewer needs-changes re-entering its implementer",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: "implement", Attempt: 1},
			want:   true,
			why: "#3942: the canonical true repass. The retry classifier declines it (not an automated " +
				"status-equals gate, not infra), and before this the classifier's veto is what withheld " +
				"the correction from the flagship implementation lane",
		},
		{
			name:   "automated policy failure re-entering its implementer",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeFail, Target: "implement", Attempt: 1},
			want:   true,
			why:    "the deterministic repass #3913 ported; unchanged",
		},
		{
			name:   "infrastructure outcome re-entering its stage",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeInfra, Target: "local-ci", Attempt: 2},
			want:   true,
			why:    "an infra repass is still a repass; the ruling is about re-entry, not about failure class",
		},
		{
			name:   "a passing gate",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomePass, Target: "push-branch", Attempt: 1},
			want:   false,
			why:    "an advancing gate has nothing to teach; the subject was accepted",
		},
		{
			name:   "an escalated needs-changes",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: "park-escalated", Escalated: true, Attempt: 3},
			want:   false,
			why: "budget exhaustion and the #316 duplicate-diff guard both resolve Escalated: the branch " +
				"is a disposition, and the stage it names is not being asked to try again",
		},
		{
			name:   "a forward branch to a stage that has not run",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: "park-needs-human", Attempt: 0},
			want:   false,
			why:    "#3929: repassAttempt 0 means the target has never run and has produced nothing to correct",
		},
		{
			name:   "a terminal @abort branch",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeFail, Target: workflow.TargetAbort, Attempt: 1},
			want:   false,
			why:    "a reserved terminal is not a stage; there is no dispatch to inject into",
		},
		{
			name:   "a terminal @escalate branch",
			branch: LearningEpisodeBranch{Outcome: gate.OutcomeFail, Target: workflow.TargetEscalate, Attempt: 1},
			want:   false,
			why:    "same, and the run is ending rather than repassing",
		},
		{
			name:   "a terminal complete branch",
			branch: LearningEpisodeBranch{Outcome: string(apiv1.VerdictNeedsChanges), Target: workflow.TerminalComplete, Attempt: 1},
			want:   false,
			why:    "same; retained verbatim from routeRetryDecision's guard set",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := LearningEpisodeAppliesToBranch(tt.branch); got != tt.want {
				t.Fatalf("LearningEpisodeAppliesToBranch(%+v) = %v, want %v — %s", tt.branch, got, tt.want, tt.why)
			}
		})
	}
}

// LearningEpisodeAppliesToBranch must remain a strict refinement of the #3929
// attempt predicate: everything it admits, LearningEpisodeAppliesToRepass
// admits too. If that ever stopped holding, #3929's ruling and #3942's
// extension would have drifted apart and the forward-branch regressions next
// door would be pinning a rule nothing enforces.
func TestLearningEpisodeBranchPredicateRefinesTheRepassPredicate(t *testing.T) {
	outcomes := []string{
		gate.OutcomePass, gate.OutcomeFail, gate.OutcomeInfra,
		string(apiv1.VerdictNeedsChanges), string(apiv1.VerdictPass),
	}
	targets := []string{
		"implement", "guard-before-implement", "park-needs-human",
		workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete,
	}
	for _, outcome := range outcomes {
		for _, target := range targets {
			for _, escalated := range []bool{false, true} {
				for attempt := -1; attempt <= 3; attempt++ {
					b := LearningEpisodeBranch{
						Outcome: outcome, Target: target, Escalated: escalated, Attempt: attempt,
					}
					if LearningEpisodeAppliesToBranch(b) && !LearningEpisodeAppliesToRepass(attempt) {
						t.Fatalf("branch %+v is admitted by LearningEpisodeAppliesToBranch but not by "+
							"LearningEpisodeAppliesToRepass; #3942 widens WHICH failure classes reach the "+
							"question, never the #3929 answer to it", b)
					}
				}
			}
		}
	}
}

// alwaysReviewer answers with a scripted sequence of verdicts, repeating the
// last one, and records every envelope it was shown — the gate-side half of the
// pointer accumulation, which is what internal/gate.readEpisodeHistory reads.
type scriptedReviewer struct {
	t        *testing.T
	verdicts []apiv1.Verdict
	calls    int
	envs     []apiv1.InvocationEnvelope
}

func (r *scriptedReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *scriptedReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.envs = append(r.envs, env)
	i := r.calls
	r.calls++
	if i >= len(r.verdicts) {
		i = len(r.verdicts) - 1
	}
	return r.verdicts[i], nil
}

// agenticRepassRun drives one agentic-reviewer fixture through a real Start()
// and returns everything the assertions below read: the implementer's
// envelopes, the reviewer's, the journal, and the run directory.
type agenticRepassRun struct {
	result        Result
	implementEnvs []apiv1.InvocationEnvelope
	reviewEnvs    []apiv1.InvocationEnvelope
	events        []journal.Event
	runDir        string
}

func runAgenticRepass(t *testing.T, runID string, machine *workflow.Machine, verdicts []apiv1.Verdict) agenticRepassRun {
	t.Helper()
	deterministic := &verdictAwareDeterministic{t: t}
	reviewer := &scriptedReviewer{t: t, verdicts: verdicts}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runDir := filepath.Join(runsDir, runID)
	return agenticRepassRun{
		result:        res,
		implementEnvs: deterministic.gotEnvs,
		reviewEnvs:    reviewer.envs,
		events:        readJournalEvents(t, runDir),
		runDir:        runDir,
	}
}

// learningInjectionAnnotations returns the learning.episode.injected
// annotations in order.
func learningInjectionAnnotations(events []journal.Event) []journal.Event {
	var out []journal.Event
	for _, e := range events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner == nil {
			continue
		}
		if kind, _ := e.Runner["kind"].(string); kind == LearningEpisodeInjectedKind {
			out = append(out, e)
		}
	}
	return out
}

func learningEpisodePointers(pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	var out []apiv1.ContextPointer
	for _, p := range pointers {
		if strings.HasPrefix(p.Name, "learning.episode[") {
			out = append(out, p)
		}
	}
	return out
}

// TestAgenticNeedsChangesRepassReceivesALearningEpisode is #3942's end-to-end
// acceptance on the local runner, driven through a real Start() over the same
// shape reference-workflows' implementation.yaml ships: an agentic `review`
// gate whose needs-changes branch re-enters `implement`.
//
// Before the fix, the repass envelope carried `review.verdict` and nothing
// else: routeRetryDecision returned retry == false (a reviewer verdict is not
// retry-classifiable), so the branch travelled walk's advance path, where no
// injection existed at all.
//
// It asserts the four surfaces the injection is measured on, and one negative:
//
//   - ROUTING is unchanged — two implement dispatches, two reviews, converging
//     completed. Widening the injection must not move the walk.
//   - CLASSIFICATION is unchanged — NO stage.retry.decision annotation. A
//     reviewer verdict is still not a policy or infrastructure failure class,
//     and priorRepassCause still reads the reviewer arm rather than a
//     fabricated stage-failure one.
//   - ARTIFACT — one learning/episode-review-<seq>.json, recorded and
//     addressed, whose bytes parse as the versioned episode schema and carry
//     the reviewer's own rationale.
//   - POINTER + INTEGRITY — the repass dispatch carries exactly one
//     learning.episode[<seq>] pointer at derived integrity, and the re-entered
//     stage's own produced integrity is downgraded to derived as a result.
func TestAgenticNeedsChangesRepassReceivesALearningEpisode(t *testing.T) {
	const runID = "run-agentic-repass-episode"
	const rationale = "reject an empty document before the token scan rather than after it"
	run := runAgenticRepass(t, runID, agenticGateMachine(t), []apiv1.Verdict{
		{Decision: apiv1.VerdictNeedsChanges, Summary: "the parser still accepts empty input", Rationale: rationale},
		{Decision: apiv1.VerdictPass, Summary: "empty input is rejected now"},
	})

	// (1) Routing: unchanged by the widening.
	if run.result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — the injection must not change where the walk goes", run.result.Phase)
	}
	if len(run.implementEnvs) != 2 || len(run.reviewEnvs) != 2 {
		t.Fatalf("dispatches = %d implement + %d review, want 2 + 2 (exactly one repass)",
			len(run.implementEnvs), len(run.reviewEnvs))
	}

	// (2) Classification: unchanged. This is the scope claim — #3942 widened
	// the episode, never retryFailureClassForGateResult. A retry-decision
	// annotation here would assert a policy/infra class over a reviewer
	// verdict, which is exactly the conflation priorRepassCause reads back.
	if got := forwardRetryDecisions(run.events); len(got) != 0 {
		t.Fatalf("retry decision annotations = %d (%+v), want 0 — an agentic needs-changes is not a "+
			"retry-classifiable failure, and widening the injection must not make it one", len(got), got)
	}

	// (3) The annotation and its artifact.
	injections := learningInjectionAnnotations(run.events)
	if len(injections) != 1 {
		t.Fatalf("learning episode injections = %d, want exactly 1 — the reviewer sent implement back, "+
			"which is the canonical repass the correction exists for", len(injections))
	}
	annotation := injections[0]
	if annotation.Stage != "implement" {
		t.Fatalf("annotation attributed to stage %q, want implement — the episode is evidence for the "+
			"invocation it feeds, not the one that was reviewed", annotation.Stage)
	}
	if annotation.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("annotation integrity = %q, want %q", annotation.Integrity, apiv1.IntegrityDerived)
	}
	if got, _ := annotation.Runner["target"].(string); got != "implement" {
		t.Fatalf("annotation target = %q, want implement", got)
	}
	if got, _ := annotation.Runner["gate"].(string); got != "review" {
		t.Fatalf("annotation gate = %q, want review", got)
	}
	if got, _ := annotation.Runner["correctionFeedback"].(string); got != rationale {
		t.Fatalf("annotation correctionFeedback = %q, want the reviewer's own rationale %q — a synthesized "+
			"fallback here would mean the verdict arm of BuildLearningEpisode was bypassed", got, rationale)
	}
	for _, key := range []string{
		"episodeId", "sourceRunId", "sourceSeq", "sourceAttempt", "nextAttempt",
		"signature", "classification", "recommendedAction", "findingIdentities",
		"episodePath", "episodeDigest",
	} {
		if _, ok := annotation.Runner[key]; !ok {
			t.Errorf("annotation is missing %q; both drivers' operator surfaces read this payload", key)
		}
	}
	if !strings.HasPrefix(annotation.Name, "learning/episode-review-") {
		t.Fatalf("episode artifact name = %q, want a learning/episode-review-<seq>.json name", annotation.Name)
	}
	if annotation.Ref == nil || annotation.Ref.Digest == "" {
		t.Fatalf("episode annotation carries no addressed ref: %+v", annotation.Ref)
	}
	var recorded bool
	for _, e := range run.events {
		if e.Type == journal.EventArtifactRecorded && e.Name == annotation.Name {
			recorded = true
			if e.Ref == nil || e.Ref.Digest != annotation.Ref.Digest {
				t.Fatalf("artifact.recorded digest %+v does not match the annotation's %q; a reader "+
					"cannot join them", e.Ref, annotation.Ref.Digest)
			}
		}
	}
	if !recorded {
		t.Fatalf("no artifact.recorded event for %q; the annotation names bytes the journal never committed",
			annotation.Name)
	}

	// (4) The pointer the repass was actually dispatched with, and the
	// integrity downgrade that follows from it.
	if got := learningEpisodePointers(run.implementEnvs[0].ContextPointers); len(got) != 0 {
		t.Fatalf("the FIRST implement dispatch carries %d learning pointer(s), want 0 — no gate has "+
			"evaluated yet: %+v", len(got), got)
	}
	repassPointers := learningEpisodePointers(run.implementEnvs[1].ContextPointers)
	if len(repassPointers) != 1 {
		t.Fatalf("the repass dispatch carries %d learning pointer(s), want exactly 1; got context pointers %+v",
			len(repassPointers), run.implementEnvs[1].ContextPointers)
	}
	pointer := repassPointers[0]
	if pointer.Integrity != apiv1.IntegrityDerived || pointer.Artifact == nil ||
		pointer.Artifact.Integrity != apiv1.IntegrityDerived || pointer.Artifact.MediaType != "application/json" {
		t.Fatalf("repass learning pointer = %+v, want the recorded ref at derived integrity", pointer)
	}
	if pointer.Artifact.Digest != annotation.Ref.Digest {
		t.Fatalf("pointer digest %q != annotation digest %q; the pointer must address the committed bytes",
			pointer.Artifact.Digest, annotation.Ref.Digest)
	}
	grades := implementFinishedIntegrity(run.events)
	if len(grades) != 2 || grades[0] != apiv1.IntegrityTrusted || grades[1] != apiv1.IntegrityDerived {
		t.Fatalf("implement stage.finished integrity = %v, want [trusted derived] — the injected pointer is "+
			"derived and produced integrity is the floor of a stage's inputs, so the repass grades derived; "+
			"that downgrade is admission control", grades)
	}
}

func implementFinishedIntegrity(events []journal.Event) []apiv1.Integrity {
	var out []apiv1.Integrity
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage == "implement" {
			out = append(out, e.Integrity)
		}
	}
	return out
}

// TestAgenticRepassEpisodeIsReadableByFindingReconciliation is the ARTIFACT
// half: injecting bytes nothing can read back would be worse than injecting
// nothing, because the correction would travel and be silently ignored.
//
// internal/gate.readEpisodeHistory selects episodes with
// apiv1.ClassifyContextPointer and then requires
// `episode.Schema == learning.EpisodeSchema && episode.Gate == gateName`. This
// asserts all three against the bytes the run actually committed, and asserts
// the re-review dispatch carries the pointer at all — the gate reconciles over
// the pointer set it is SHOWN, so a pointer that reached the implementer but
// not the gate would leave the lifecycle (resolved/suppressed/reopened,
// E4-E9's disprove logic) permanently empty on this lane.
func TestAgenticRepassEpisodeIsReadableByFindingReconciliation(t *testing.T) {
	const runID = "run-agentic-repass-artifact"
	run := runAgenticRepass(t, runID, agenticGateMachine(t), []apiv1.Verdict{
		{
			Decision: apiv1.VerdictNeedsChanges, Summary: "the parser still accepts empty input",
			Rationale: "reject an empty document before the token scan",
			Findings: []apiv1.Finding{{
				Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError,
				Location: "internal/parse/doc.go:41", Message: "empty input is accepted",
			}},
		},
		{Decision: apiv1.VerdictPass, Summary: "empty input is rejected now"},
	})
	if run.result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", run.result.Phase)
	}
	if len(run.reviewEnvs) != 2 {
		t.Fatalf("reviewer dispatches = %d, want 2", len(run.reviewEnvs))
	}

	// The RE-REVIEW must see the episode: that is the pointer set
	// reconcileLearningFindings runs over.
	gatePointers := learningEpisodePointers(run.reviewEnvs[1].ContextPointers)
	if len(gatePointers) != 1 {
		t.Fatalf("the second review dispatch carries %d learning pointer(s), want 1 — finding "+
			"reconciliation runs over the pointers the GATE is shown: %+v",
			len(gatePointers), run.reviewEnvs[1].ContextPointers)
	}
	pointer := gatePointers[0]
	if class, _ := apiv1.ClassifyContextPointer(pointer.Name); class != apiv1.ContextPointerLearningEpisode {
		t.Fatalf("pointer %q classifies as %q, want %q — this is the exact selector "+
			"internal/gate.readEpisodeHistory uses, so a name it rejects is an episode no reconciliation "+
			"can ever read", pointer.Name, class, apiv1.ContextPointerLearningEpisode)
	}
	if pointer.Artifact == nil {
		t.Fatalf("learning pointer %q carries no artifact", pointer.Name)
	}
	data, err := pointer.Artifact.Resolve(run.runDir)
	if err != nil {
		t.Fatalf("resolve episode artifact: %v", err)
	}
	var episode learning.Episode
	if err := json.Unmarshal(data, &episode); err != nil {
		t.Fatalf("the injected episode does not parse: %v\n%s", err, data)
	}
	if episode.Schema != learning.EpisodeSchema {
		t.Fatalf("episode schema = %q, want %q — readEpisodeHistory drops any other value",
			episode.Schema, learning.EpisodeSchema)
	}
	if episode.Gate != "review" {
		t.Fatalf("episode gate = %q, want review — readEpisodeHistory drops episodes belonging to "+
			"another gate", episode.Gate)
	}
	if episode.SourceRunID != runID || episode.Stage != "implement" {
		t.Fatalf("episode identity = run %q stage %q, want %q/implement", episode.SourceRunID, episode.Stage, runID)
	}
	if episode.Outcome != learning.OutcomeUnresolved {
		t.Fatalf("episode outcome = %q, want %q at injection time", episode.Outcome, learning.OutcomeUnresolved)
	}
	if len(episode.Findings) == 0 {
		t.Fatal("episode carries no findings; identity correlation across attempts has nothing to key on")
	}
	for i, finding := range episode.Findings {
		if finding.ID == "" || finding.LearningSignature == "" || finding.LearningClassification == "" {
			t.Fatalf("episode finding %d is not normalized: %+v — the ID and signature ARE the "+
				"cross-run identity readEpisodeHistory correlates on", i, finding)
		}
		if finding.LearningSignature != learning.FindingSignature("review", finding) {
			t.Fatalf("episode finding %d signature %q is not the canonical signature for its content",
				i, finding.LearningSignature)
		}
	}
	if len(episode.Actions) != len(episode.Findings) {
		t.Fatalf("episode has %d finding-level actions for %d findings; the authoritative slice must "+
			"cover every finding", len(episode.Actions), len(episode.Findings))
	}
	if episode.ID != learning.EpisodeID(episode) {
		t.Fatalf("episode ID %q is not the content address of its own bytes", episode.ID)
	}
}

// TestAgenticGateInjectsNoEpisodeOnTerminalOrEscalatedBranches is the NEGATIVE
// half, and it is what keeps #3942 from being "inject whenever a reviewer says
// needs-changes".
//
// Two shapes, both reachable from the very same agentic gate:
//
//   - a `fail` verdict routing to @abort. Reserved terminal: there is no
//     dispatch to inject into, and the run is over.
//   - a needs-changes loop that exhausts the repass budget. The final
//     evaluation resolves Escalated, and an escalated branch is a disposition:
//     the stage it names is being parked, not asked to try again. Injecting
//     there would assert a correction, under a content-addressed signature
//     readEpisodeHistory correlates across runs, against work nobody will do.
func TestAgenticGateInjectsNoEpisodeOnTerminalOrEscalatedBranches(t *testing.T) {
	t.Run("fail routes to a reserved terminal", func(t *testing.T) {
		run := runAgenticRepass(t, "run-agentic-terminal", agenticGateMachine(t), []apiv1.Verdict{
			{Decision: apiv1.VerdictFail, Summary: "unscopeable", Rationale: "this needs a human"},
		})
		if run.result.Phase != journal.PhaseAborted {
			t.Fatalf("phase = %q, want aborted (the fail branch is @abort)", run.result.Phase)
		}
		if got := learningInjectionAnnotations(run.events); len(got) != 0 {
			t.Fatalf("learning episode injections = %d, want 0 — @abort is not a stage, and the run is "+
				"over: there is no repass to correct", len(got))
		}
		assertNoEpisodeArtifacts(t, run.events)
	})

	t.Run("an exhausted repass budget escalates without a final injection", func(t *testing.T) {
		run := runAgenticRepass(t, "run-agentic-escalated", agenticEscalatingGateMachine(t), []apiv1.Verdict{
			{Decision: apiv1.VerdictNeedsChanges, Summary: "still wrong", Rationale: "not fixed yet"},
		})
		if run.result.Phase != journal.PhaseEscalated {
			t.Fatalf("phase = %q, want escalated (the budget must run out)", run.result.Phase)
		}
		injections := learningInjectionAnnotations(run.events)
		// Every charged repass owes a correction; the ESCALATING evaluation
		// does not, so injections must be one fewer than the reviewer's
		// non-pass verdicts.
		if len(injections) != len(run.implementEnvs)-1 {
			t.Fatalf("learning episode injections = %d for %d implement dispatches, want %d — one per "+
				"repass actually dispatched, and none for the escalation that ended the run",
				len(injections), len(run.implementEnvs), len(run.implementEnvs)-1)
		}
		if len(injections) == 0 {
			t.Fatal("the fixture escalated without ever repassing; this sub-test would then be vacuous")
		}
		for _, injection := range injections {
			if target, _ := injection.Runner["target"].(string); target != "implement" {
				t.Fatalf("injection targets %q, want implement — no correction may be addressed to the "+
					"escalation branch", target)
			}
		}
	})
}

func assertNoEpisodeArtifacts(t *testing.T, events []journal.Event) {
	t.Helper()
	for _, e := range events {
		if strings.HasPrefix(e.Name, "learning/episode-") {
			t.Fatalf("seq %d recorded a learning episode artifact %q", e.Seq, e.Name)
		}
	}
}

// agenticEscalatingGateMachine is agenticGateMachine with an explicit escalate
// control branch, so a budget-exhausted reviewer loop ends at a disposition
// this test can name rather than at the bare @escalate terminal.
func agenticEscalatingGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review",
			},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          workflow.TerminalComplete,
					"needs-changes": "implement",
					"fail":          workflow.TargetAbort,
					"escalate":      workflow.TargetEscalate,
				},
			},
		},
	}
	m, err := workflow.Compile(
		workflow.Definition{Name: "agentic-escalating-fixture", Version: 1, Spec: spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile agentic escalating machine: %v", err)
	}
	return m
}
