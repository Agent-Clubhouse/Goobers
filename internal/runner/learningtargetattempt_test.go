package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/runsemantics"

	// Coverage for #3931: a learning episode's nextAttempt — and the attempt its
	// annotation is journaled under — belong to the stage being RE-ENTERED, not to
	// the stage that failed.
	//
	// Every learning-episode fixture in the tree before this one walked
	// `implement -> review -> implement`, the TRIVIAL send-back, where the subject
	// and the target are the same stage and subjectAttempt+1 happens to equal the
	// target's next attempt. That is why nothing caught the defect: the two
	// derivations are indistinguishable on the only shape anyone had written down.
	//
	// The fixtures here walk a NONTRIVIAL send-back — a gate whose fail branch
	// re-enters a stage other than its subject, which is the shape every shipped
	// lane actually uses (implementation.yaml's `local-gate: fail -> implement`
	// over a `local-ci` subject, `ci-gate: fail -> remediate-ci` over `ci-poll`,
	// pr-remediation's `finding-responses-gate: fail -> guard-before-implement`
	// over `validate-finding-responses`) — and one where the target's attempt count
	// has already advanced past the subject's, which is what makes the two numbers
	// observably different.
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/workflow"
)

// The end-to-end statement on the local runner, through a real Start().
//
// nontrivialSendBackMachine walks implement -> review -> local-ci -> local-gate
// with BOTH gates' fail branches re-entering implement, which is the shipped
// implementation lane's shape reduced to its skeleton. The run takes two
// repasses:
//
//	implement#1 fails      -> review     fails -> implement (subject implement@1)
//	implement#2 succeeds
//	local-ci#1 fails       -> local-gate fails -> implement (subject local-ci@1)
//	implement#3 succeeds, local-ci#2 succeeds, done
//
// The first is the trivial send-back and is unchanged: subject and target are
// the same stage, so 1+1 and "implement's next attempt" are both 2. The second
// is the whole issue: local-ci runs once per cycle so its attempt is 1
// essentially always, while implement is about to take its THIRD attempt. The
// pre-change code told implement it was on attempt 2 and journaled the
// annotation as implement/2 — an attempt that had already happened, with
// different content.
func TestLearningEpisodeAddressesTheTargetsOwnNextAttempt(t *testing.T) {
	const runID = "run-nontrivial-send-back"
	capture := newEnvelopeCapture()
	script := newScriptedDeterministic(map[string][]stubTaskResult{
		runID + ":implement": {
			{status: apiv1.ResultFailure, summary: "3 tests failed",
				errorInfo: &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "exit status 1", Retryable: true}},
			{status: apiv1.ResultSuccess},
			{status: apiv1.ResultSuccess},
		},
		runID + ":local-ci": {
			{status: apiv1.ResultFailure, summary: "local ci is red",
				errorInfo: &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "exit status 1", Retryable: true}},
			{status: apiv1.ResultSuccess},
		},
	}, capture)

	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return script, nil
	}, gate.NewAutomatedEvaluator())
	r.cfg.ScratchDir = t.TempDir()

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: nontrivialSendBackMachine(t),
		Gaggle:  "acme-web",
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
	if got := countStageStarts(events, "implement"); got != 3 {
		t.Fatalf("implement started %d time(s), want 3 — the fixture must take BOTH repasses for the "+
			"target's attempt count to overtake the subject's", got)
	}

	injections := learningInjectionRecords(t, runDir, events)
	if len(injections) != 2 {
		t.Fatalf("learning injections = %d, want 2:\n%s", len(injections), formatInjections(injections))
	}

	// The trivial send-back, unchanged: subject implement@1, target implement,
	// next attempt 2. This is the arm every pre-#3931 fixture covered.
	trivial := injections[0]
	want := learningInjectionRecord{
		Gate: "review", Subject: "implement", Target: "implement",
		AnnotationStage: "implement", AnnotationAttempt: 2,
		SourceAttempt: 1, NextAttempt: 2,
	}
	if trivial.comparable() != want {
		t.Fatalf("trivial send-back injection = %+v, want %+v — #3931 must not move the degenerate case",
			trivial.comparable(), want)
	}

	// The nontrivial send-back. sourceAttempt still names the SUBJECT's
	// attempt (local-ci@1) because that is what says which failure the episode
	// is about; nextAttempt and the annotation's Attempt name the TARGET's.
	nontrivial := injections[1]
	want = learningInjectionRecord{
		Gate: "local-gate", Subject: "local-ci", Target: "implement",
		AnnotationStage: "implement", AnnotationAttempt: 3,
		SourceAttempt: 1, NextAttempt: 3,
	}
	if nontrivial.comparable() != want {
		t.Fatalf("nontrivial send-back injection = %+v, want %+v — the subject local-ci is on attempt 1 "+
			"while implement is about to take attempt 3; sourceAttempt+1 would say 2, an attempt of "+
			"implement that has already happened with different content",
			nontrivial.comparable(), want)
	}

	// The annotation's Attempt is the TARGET's own re-entry index, which is
	// also the number internal/gate charged to RepassAttempts[implement] plus
	// one. The two derivations are independent — one reads the target's entry
	// history, the other the repass budget the gate charged — so agreeing is
	// evidence rather than tautology.
	repasses := repassAttemptsFor(events, "implement")
	if len(repasses) != len(injections) {
		t.Fatalf("retry decisions targeting implement = %v, learning injections = %d; the injection "+
			"rides the retry arm, so there must be one per decision", repasses, len(injections))
	}
	for i, injection := range injections {
		if want := repasses[i] + 1; injection.AnnotationAttempt != want {
			t.Fatalf("injection %d annotated implement/%d, but the gate charged repassAttempt %d, so the "+
				"re-entry it feeds is implement/%d", i+1, injection.AnnotationAttempt, repasses[i], want)
		}
	}

	// And the third implement dispatch — the one the second episode is
	// addressed to — was actually handed it.
	third := capture.envelopes("implement")
	if len(third) != 3 {
		t.Fatalf("implement was dispatched %d time(s) with captured envelopes, want 3", len(third))
	}
	if !envelopeCarriesEpisode(third[2], nontrivial.PointerName) {
		t.Fatalf("implement attempt 3 carried pointers %v, want the episode pointer %q",
			envelopePointerNames(third[2]), nontrivial.PointerName)
	}
}

// --- fixtures and readers ---

// nontrivialSendBackMachine is the shipped implementation lane's skeleton: two
// gates whose fail branches both re-enter implement, one of them over a
// DIFFERENT subject.
func nontrivialSendBackMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	gateSpec := func(name string, branches map[string]string) apiv1.Gate {
		return apiv1.Gate{
			Name: name, Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches:  branches,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "nontrivial-send-back", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "acme-web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "implement",
			Tasks: []apiv1.Task{
				task("implement", "review"),
				task("local-ci", "local-gate"),
			},
			Gates: []apiv1.Gate{
				gateSpec("review", map[string]string{"pass": "local-ci", "fail": "implement"}),
				gateSpec("local-gate", map[string]string{"pass": workflow.TerminalComplete, "fail": "implement"}),
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile nontrivial send-back fixture: %v", err)
	}
	return machine
}

// scriptedDeterministic answers a SEQUENCE of results per task id, so a
// fixture can fail a stage on one attempt and pass it on the next. The last
// entry repeats. Locked because the concurrent parallel walker dispatches
// branches from several goroutines.
type scriptedDeterministic struct {
	mu      sync.Mutex
	byTask  map[string][]stubTaskResult
	calls   map[string]int
	capture *envelopeCapture
}

func newScriptedDeterministic(byTask map[string][]stubTaskResult, capture *envelopeCapture) *scriptedDeterministic {
	return &scriptedDeterministic{byTask: byTask, calls: map[string]int{}, capture: capture}
}

func (s *scriptedDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.mu.Lock()
	script, ok := s.byTask[env.TaskID]
	if !ok || len(script) == 0 {
		s.mu.Unlock()
		return apiv1.ResultEnvelope{}, fmt.Errorf("scripted executor: no canned output for %q", env.TaskID)
	}
	index := s.calls[env.TaskID]
	s.calls[env.TaskID]++
	if index >= len(script) {
		index = len(script) - 1
	}
	cfg := script[index]
	s.mu.Unlock()
	if s.capture != nil {
		s.capture.record(env)
	}
	return apiv1.ResultEnvelope{
		Status: cfg.status, Summary: cfg.summary, Error: cfg.errorInfo, Outputs: cfg.outputs,
	}, nil
}

// envelopeCapture records what each dispatch was actually HANDED, which is the
// only place an episode pointer becomes visible to a goober.
type envelopeCapture struct {
	mu   sync.Mutex
	seen []apiv1.InvocationEnvelope
}

func newEnvelopeCapture() *envelopeCapture { return &envelopeCapture{} }

func (c *envelopeCapture) record(env apiv1.InvocationEnvelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, env)
}

// envelopes returns, in dispatch order, the envelopes whose task id names
// stage.
func (c *envelopeCapture) envelopes(stage string) []apiv1.InvocationEnvelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []apiv1.InvocationEnvelope
	for _, env := range c.seen {
		if strings.HasSuffix(env.TaskID, ":"+stage) {
			out = append(out, env)
		}
	}
	return out
}

func envelopePointerNames(env apiv1.InvocationEnvelope) []string {
	out := make([]string, 0, len(env.ContextPointers))
	for _, pointer := range env.ContextPointers {
		out = append(out, pointer.Name)
	}
	return out
}

func envelopeCarriesEpisode(env apiv1.InvocationEnvelope, name string) bool {
	for _, pointer := range env.ContextPointers {
		if pointer.Name == name {
			return true
		}
	}
	return false
}

// learningInjectionRecord is one injection reduced to the numbers #3931 is
// about, read off BOTH the annotation and the episode bytes so the two cannot
// disagree.
type learningInjectionRecord struct {
	Gate              string
	Subject           string
	Target            string
	AnnotationStage   string
	AnnotationAttempt int
	SourceAttempt     int
	NextAttempt       int
	PointerName       string
	Branch            int
}

// comparable drops the fields that are identity rather than arithmetic, so a
// failure message reads as the claim under test.
func (r learningInjectionRecord) comparable() learningInjectionRecord {
	r.PointerName = ""
	r.Branch = 0
	return r
}

func learningInjectionRecords(t *testing.T, runDir string, events []journal.Event) []learningInjectionRecord {
	t.Helper()
	var out []learningInjectionRecord
	for _, e := range events {
		if e.Type != journal.EventRunnerAnnotation || e.Runner == nil {
			continue
		}
		if kind, _ := e.Runner["kind"].(string); kind != runsemantics.LearningEpisodeInjectedKind {
			continue
		}
		if e.Ref == nil {
			t.Fatalf("learning annotation at seq %d carries no artifact ref", e.Seq)
		}
		pointer := apiv1.ArtifactPointer{Path: e.Ref.Path, Digest: e.Ref.Digest, Size: e.Ref.Size}
		data, err := pointer.Resolve(runDir)
		if err != nil {
			t.Fatalf("resolve episode artifact %q: %v", e.Name, err)
		}
		var episode learning.Episode
		if err := json.Unmarshal(data, &episode); err != nil {
			t.Fatalf("decode episode artifact %q: %v", e.Name, err)
		}
		target, _ := e.Runner["target"].(string)
		// The annotation payload and the bytes must agree; reading the
		// arithmetic off the BYTES is what makes this a digest assertion.
		if got, _ := runsemantics.RunnerInt(e.Runner["nextAttempt"]); got != episode.NextAttempt {
			t.Fatalf("annotation nextAttempt %d disagrees with the episode's %d for %q",
				got, episode.NextAttempt, e.Name)
		}
		out = append(out, learningInjectionRecord{
			Gate:              episode.Gate,
			Subject:           episode.Stage,
			Target:            target,
			AnnotationStage:   e.Stage,
			AnnotationAttempt: e.Attempt,
			SourceAttempt:     episode.SourceAttempt,
			NextAttempt:       episode.NextAttempt,
			PointerName:       runsemantics.LearningEpisodePointerName(episode.SourceSeq),
			Branch:            e.Branch,
		})
	}
	return out
}

// repassAttemptsFor returns, in order, the repassAttempt internal/gate charged
// for each retry decision that sends work back to stage. It is the independent
// second derivation of the same counter: the gate charges it against its own
// repass budget while the episode addressing reads the target's entry history.
func repassAttemptsFor(events []journal.Event, stage string) []int {
	out := []int{}
	for _, decision := range forwardRetryDecisions(events) {
		if decision.Target == stage {
			out = append(out, decision.RepassAttempt)
		}
	}
	return out
}

func formatInjections(records []learningInjectionRecord) string {
	if len(records) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(records))
	for _, r := range records {
		parts = append(parts, fmt.Sprintf("%+v", r))
	}
	return strings.Join(parts, "\n")
}
