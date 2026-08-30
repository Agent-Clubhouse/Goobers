package engine

// hitl_test.go is the acceptance surface for #3883 (decision 005 R8): the
// versioned Temporal protocol through which an operator resolves an
// escalation, reruns a stage, or resumes from a terminal on an ENGINE-DRIVEN
// run.
//
// Every test here is written against the issue's far-side evidence — "an
// escalated engine-driven run is resolved by an operator action; the workflow
// resumes at the correct stage; the journal records the resolution and the
// resuming attempt; no in-process runner path is invoked" — and against the
// in-process runner semantics the protocol must not drift from
// (internal/runner/resume.go, internal/runner/rerun.go).
//
// The replay half (a real worker.WorkflowReplayer over a history that
// CONTAINS accepted updates) lives in hitlreplay_test.go, which needs a dev
// server.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// hitlEscalatingSpec is the shape every test in this file escalates on: an
// agentic implementer, an agentic reviewer whose fail branch escalates, and a
// second stage the operator's resolution can route the run to. It is
// deliberately the shape merge-review and pr-remediation have — the two lanes
// #3883 blocks.
func hitlEscalatingSpec() apiv1.WorkflowSpec {
	return fixtureSpec("implement",
		[]apiv1.Task{
			agenticTask("implement", "review"),
			agenticTask("ship", wf.TerminalComplete),
		},
		[]apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic,
			Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{
				"pass":          "ship",
				"fail":          wf.TargetAbort,
				"needs-changes": wf.TargetEscalate,
			},
		}},
	)
}

// hitlTwiceEscalatingSpec adds a second gate that escalates again, so the run
// is still alive — and parked on a second operator hold — when a late delivery
// arrives. The idempotency tests need that: a duplicate landing after the run
// had already settled would prove nothing about deduplication.
func hitlTwiceEscalatingSpec() apiv1.WorkflowSpec {
	spec := hitlEscalatingSpec()
	spec.Tasks[1].Next = "verify"
	spec.Gates = append(spec.Gates, apiv1.Gate{
		Name: "verify", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
		Branches: map[string]string{
			"pass":          wf.TerminalComplete,
			"fail":          wf.TargetAbort,
			"needs-changes": wf.TargetEscalate,
		},
	})
	return spec
}

// hitlInputFor builds a HITL-enabled run over an explicit spec.
func hitlInputFor(t *testing.T, spec apiv1.WorkflowSpec, actors ...string) RunInput {
	t.Helper()
	in := runInput("hitl", spec)
	in.RunID = "run-hitl"
	in.TriggerKind = string(journal.TriggerManual)
	in.HITL = &HITLPolicy{Enabled: true, WaitSeconds: 3600, Actors: actors}
	return in
}

// hitlTwiceFailingExec escalates at both gates.
func hitlTwiceFailingExec() *scriptedExec {
	exec := hitlFailingExec()
	exec.verdicts["verify"] = []apiv1.Verdict{{Decision: apiv1.VerdictNeedsChanges, Summary: "still not there"}}
	return exec
}

// hitlInput builds a HITL-enabled run over hitlEscalatingSpec.
func hitlInput(t *testing.T, actors ...string) RunInput {
	t.Helper()
	in := runInput("hitl", hitlEscalatingSpec())
	in.RunID = "run-hitl"
	in.TriggerKind = string(journal.TriggerManual)
	in.HITL = &HITLPolicy{Enabled: true, WaitSeconds: 3600, Actors: actors}
	return in
}

// hitlFailingExec escalates: the implementer succeeds, the reviewer fails.
func hitlFailingExec() *scriptedExec {
	exec := newScriptedExec(map[string][]scriptedCall{
		"implement": {{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}}},
		"ship":      {{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "shipped"}}},
	})
	exec.verdicts = map[string][]apiv1.Verdict{
		"review": {{Decision: apiv1.VerdictNeedsChanges, Summary: "needs work"}},
	}
	return exec
}

// hitlEnv wires a test environment for the fixture.
func hitlEnv(t *testing.T, exec *scriptedExec) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC))
	env.RegisterActivity(&Activities{
		Goober:     exec,
		Det:        exec,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
	})
	return env
}

// hitlOutcome captures what one update delivery answered with.
type hitlOutcome struct {
	rejected error
	accepted bool
	ack      HITLAck
	err      error
}

func (o *hitlOutcome) callbacks(t *testing.T) *testsuite.TestUpdateCallback {
	t.Helper()
	return &testsuite.TestUpdateCallback{
		OnAccept: func() { o.accepted = true },
		OnReject: func(err error) { o.rejected = err },
		OnComplete: func(value interface{}, err error) {
			o.err = err
			if err != nil {
				return
			}
			if value == nil {
				return
			}
			// The test env hands the handler's return value back through the
			// data converter, so decode rather than type-asserting: a silent
			// assertion miss would leave every ack assertion vacuous.
			switch v := value.(type) {
			case HITLAck:
				o.ack = v
			case *HITLAck:
				o.ack = *v
			case converter.EncodedValue:
				if derr := v.Get(&o.ack); derr != nil {
					t.Errorf("decode ack: %v", derr)
				}
			default:
				t.Errorf("unexpected update result type %T", value)
			}
		},
	}
}

// deliver schedules one intent at delay and returns the outcome it collects.
func deliver(t *testing.T, env *testsuite.TestWorkflowEnvironment, delay time.Duration, updateID string, intent HITLIntent) *hitlOutcome {
	t.Helper()
	out := &hitlOutcome{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(HITLUpdateName, updateID, out.callbacks(t), intent)
	}, delay)
	return out
}

func baseIntent(kind HITLIntentKind, requestID string) HITLIntent {
	return HITLIntent{
		Protocol:                   HITLProtocol,
		Version:                    HITLProtocolVersion,
		Kind:                       kind,
		RunID:                      "run-hitl",
		RequestID:                  requestID,
		Actor:                      "ops@example.com",
		ExpectedTerminalGeneration: 1,
	}
}

func hitlProjection(t *testing.T, env *testsuite.TestWorkflowEnvironment) JournalProjection {
	t.Helper()
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query journal projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode journal projection: %v", err)
	}
	return proj
}

func hitlEvents(proj JournalProjection) []journal.Event {
	events := make([]journal.Event, 0, len(proj.Ops))
	for _, op := range proj.Ops {
		if op.Kind == opAppend && op.Event != nil {
			events = append(events, *op.Event)
		}
	}
	return events
}

func hitlEventTypes(events []journal.Event) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, string(e.Type))
	}
	return types
}

func hitlCountEvents(events []journal.Event, kind journal.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == kind {
			n++
		}
	}
	return n
}

func hitlFindEvent(events []journal.Event, kind journal.EventType) (journal.Event, bool) {
	for _, e := range events {
		if e.Type == kind {
			return e, true
		}
	}
	return journal.Event{}, false
}

// TestHITLDisabledRunSettlesUnchanged is the invariance test. A run whose
// input declares no HITL policy — every run started before #3883, and every
// lane with no human gate — must escalate exactly as it did before: one
// terminal, no hold, no operator-protocol event.
//
// It deliberately delivers NO intent. A run with no policy never parks, so
// there is no instant in its execution a test-environment delayed callback can
// reliably be scheduled at; asserting the refusal here would be a coin flip on
// whether the mock clock ever advanced. The refusal itself is pinned
// deterministically twice over: at the session seam by
// TestHITLNotEnabledRefusalIsNamed, and end to end against a real server by
// TestHITLPreProtocolHistoryReplays.
func TestHITLDisabledRunSettlesUnchanged(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)
	in := hitlInput(t)
	in.HITL = nil

	env.ExecuteWorkflow(Run, in)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete; a run with no HITL policy must not hold its terminal open")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q", res.Status, StatusEscalated)
	}
	events := hitlEvents(hitlProjection(t, env))
	if got := hitlCountEvents(events, journal.EventRunFinished); got != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1 (types: %v)", got, hitlEventTypes(events))
	}
	if got := hitlCountEvents(events, journal.EventRunResumed); got != 0 {
		t.Fatalf("run.resumed count = %d, want 0 on a run with no HITL policy", got)
	}
}

// TestHITLNotEnabledRefusalIsNamed pins the refusal a run with no pinned
// policy answers with, deterministically and without a workflow: the operator
// is told the run is not listening, not that their payload was malformed and
// not that the run is merely busy.
func TestHITLNotEnabledRefusalIsNamed(t *testing.T) {
	session := &hitlSession{runID: "run-hitl", policy: nil}
	// An intent that is ALSO ill-formed for its kind (no target, no complete),
	// so the ordering is pinned too: "this run does not speak the protocol"
	// outranks "fix your payload", because fixing the payload changes nothing.
	err := session.validate(baseIntent(HITLResumeFromTerminal, "req-1"))
	code, message, ok := HITLRefusalCode(err)
	if !ok || code != HITLErrNotAcceptingUp {
		t.Fatalf("refusal = (%q, %v) for %v, want %q", code, ok, err, HITLErrNotAcceptingUp)
	}
	if !strings.Contains(message, "run-hitl") {
		t.Fatalf("refusal message = %q, want it to name the run", message)
	}
	if session.phase == hitlPhaseAwaiting {
		t.Fatal("validating an intent moved the session's phase; the validator must not mutate state")
	}
}

// TestHITLResolveEscalationResumesAtBranchTarget is the issue's far-side
// evidence: an escalated engine-driven run is resolved by an operator, the
// workflow resumes at the state the gate's own branch names, and the journal
// records both the resolution and the resuming attempt.
func TestHITLResolveEscalationResumesAtBranchTarget(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResolveEscalation, "req-resolve")
	intent.Gate = "review"
	intent.Resolution = HITLResolutionOverride
	intent.Decision = "pass"
	intent.Rationale = "reviewed by hand; the finding is a false positive"
	out := deliver(t, env, time.Minute, "req-resolve", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected != nil {
		t.Fatalf("intent rejected: %v", out.rejected)
	}
	if out.err != nil {
		t.Fatalf("intent failed: %v", out.err)
	}
	if !out.ack.Resumed || out.ack.ResumeState != "ship" {
		t.Fatalf("ack = %+v, want a resume at %q", out.ack, "ship")
	}
	if out.ack.Protocol != HITLProtocol || out.ack.Version != HITLProtocolVersion {
		t.Fatalf("ack protocol = %s/%d, want %s/%d", out.ack.Protocol, out.ack.Version, HITLProtocol, HITLProtocolVersion)
	}

	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q — the resolved run must finish through ship", res.Status, StatusCompleted)
	}

	events := hitlEvents(hitlProjection(t, env))
	// Runner parity: the escalated terminal is journaled, THEN the resume,
	// THEN the new terminal — the exact sequence internal/runner writes for
	// ResumeFromTerminal.
	if got := hitlCountEvents(events, journal.EventRunFinished); got != 2 {
		t.Fatalf("run.finished count = %d, want 2 (escalated then completed): %v", got, hitlEventTypes(events))
	}
	resumed, ok := hitlFindEvent(events, journal.EventRunResumed)
	if !ok {
		t.Fatalf("no run.resumed event: %v", hitlEventTypes(events))
	}
	if resumed.Actor != "ops@example.com" {
		t.Fatalf("run.resumed actor = %q, want the caller identity", resumed.Actor)
	}
	if resumed.Action != HITLResolutionOverride {
		t.Fatalf("run.resumed action = %q, want %q", resumed.Action, HITLResolutionOverride)
	}
	if resumed.Gate != "review" || resumed.Decision != "pass" {
		t.Fatalf("run.resumed gate/decision = %q/%q, want review/pass", resumed.Gate, resumed.Decision)
	}
	if resumed.Rationale == "" {
		t.Fatalf("run.resumed dropped the operator's rationale")
	}
	if resumed.Target != "ship" {
		t.Fatalf("run.resumed target = %q, want ship", resumed.Target)
	}
	if resumed.Status != StatusEscalated {
		t.Fatalf("run.resumed status = %q, want the terminal it resumed from", resumed.Status)
	}
	if resumed.Runner["idempotencyKey"] != "req-resolve" {
		t.Fatalf("run.resumed provenance = %v, want the request id", resumed.Runner)
	}
	// The resuming attempt actually ran.
	if exec.calls["ship"] != 1 {
		t.Fatalf("ship dispatched %d times, want exactly 1 after the resolution", exec.calls["ship"])
	}
	// Ordering: the run.resumed must sit between the two terminals.
	seq := hitlEventTypes(events)
	firstFinished, resumedAt, lastFinished := -1, -1, -1
	for i, ty := range seq {
		switch journal.EventType(ty) {
		case journal.EventRunFinished:
			if firstFinished < 0 {
				firstFinished = i
			}
			lastFinished = i
		case journal.EventRunResumed:
			resumedAt = i
		}
	}
	if firstFinished >= resumedAt || resumedAt >= lastFinished {
		t.Fatalf("event order = %v, want run.finished < run.resumed < run.finished", seq)
	}
}

// TestHITLRerunStageJournalsHumanAttempt covers the rerun intent against
// internal/runner's RerunStage semantics: the addendum is required, the
// attempt is the CUMULATIVE next attempt (nextRerunAttempt's arithmetic), the
// class is journal.AttemptHuman, and the re-dispatched stage actually receives
// the addendum.
func TestHITLRerunStageJournalsHumanAttempt(t *testing.T) {
	exec := hitlFailingExec()
	// The reruns implementer passes review the second time round.
	exec.verdicts = map[string][]apiv1.Verdict{
		"review": {
			{Decision: apiv1.VerdictNeedsChanges, Summary: "needs work"},
			{Decision: apiv1.VerdictPass, Summary: "fixed"},
		},
	}
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLRerunStage, "req-rerun")
	intent.Stage = "implement"
	intent.InstructionAddendum = "address the reviewer's finding about the nil check"
	out := deliver(t, env, time.Minute, "req-rerun", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected != nil {
		t.Fatalf("rerun intent rejected: %v", out.rejected)
	}
	if out.err != nil {
		t.Fatalf("rerun intent failed: %v", out.err)
	}
	if !out.ack.Resumed || out.ack.ResumeState != "implement" {
		t.Fatalf("ack = %+v, want a resume at implement", out.ack)
	}
	if out.ack.Attempt != 2 {
		t.Fatalf("ack attempt = %d, want 2 (one past the single prior start)", out.ack.Attempt)
	}

	events := hitlEvents(hitlProjection(t, env))
	req, ok := hitlFindEvent(events, journal.EventStageRerunRequested)
	if !ok {
		t.Fatalf("no stage.rerun.requested event: %v", hitlEventTypes(events))
	}
	if req.Stage != "implement" {
		t.Fatalf("rerun stage = %q, want implement", req.Stage)
	}
	if req.Attempt != 2 {
		t.Fatalf("rerun attempt = %d, want 2 — nextRerunAttempt's cumulative count", req.Attempt)
	}
	if req.AttemptClass != journal.AttemptHuman {
		t.Fatalf("rerun attempt class = %q, want %q", req.AttemptClass, journal.AttemptHuman)
	}
	if req.Actor != "ops@example.com" {
		t.Fatalf("rerun actor = %q, want the caller identity", req.Actor)
	}
	if req.InstructionAddendum != intent.InstructionAddendum {
		t.Fatalf("rerun addendum = %q, want the operator's", req.InstructionAddendum)
	}
	if exec.calls["implement"] != 2 {
		t.Fatalf("implement dispatched %d times, want 2 (original plus the operator's rerun)", exec.calls["implement"])
	}
	if got := exec.addenda["implement"]; got != intent.InstructionAddendum {
		t.Fatalf("re-dispatched implement received addendum %q, want the operator's", got)
	}
}

// TestHITLResumeFromTerminalComplete covers the raw resume intent's Complete
// form: the run finishes at @complete without executing another stage, exactly
// as ResumeFromTerminalInput.Complete does.
func TestHITLResumeFromTerminalComplete(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResumeFromTerminal, "req-complete")
	intent.Complete = true
	out := deliver(t, env, time.Minute, "req-complete", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected != nil {
		t.Fatalf("resume intent rejected: %v", out.rejected)
	}
	if !out.ack.Resumed || out.ack.ResumeState != wf.TerminalComplete {
		t.Fatalf("ack = %+v, want a resume at %s", out.ack, wf.TerminalComplete)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", res.Status, StatusCompleted)
	}
	if exec.calls["ship"] != 0 {
		t.Fatalf("ship dispatched %d times, want 0 — a completing resume runs nothing", exec.calls["ship"])
	}
	events := hitlEvents(hitlProjection(t, env))
	resumed, ok := hitlFindEvent(events, journal.EventRunResumed)
	if !ok {
		t.Fatalf("no run.resumed event: %v", hitlEventTypes(events))
	}
	if !resumed.Complete || resumed.Target != "" {
		t.Fatalf("run.resumed complete/target = %t/%q, want true/\"\"", resumed.Complete, resumed.Target)
	}
}

// TestHITLDenyKeepsTerminal covers the deny resolution: the escalation was
// reviewed and the run deliberately stays terminal. It is journaled — actor,
// rationale, idempotency key, under the SAME escalation.resolution marker the
// in-process path uses — and it resumes nothing.
func TestHITLDenyKeepsTerminal(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResolveEscalation, "req-deny")
	intent.Gate = "review"
	intent.Resolution = HITLResolutionDeny
	intent.Rationale = "the reviewer is right; this stays escalated"
	out := deliver(t, env, time.Minute, "req-deny", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected != nil || out.err != nil {
		t.Fatalf("deny refused: rejected=%v err=%v", out.rejected, out.err)
	}
	if out.ack.Resumed {
		t.Fatal("a deny must not resume the run")
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q — a denied escalation stays terminal", res.Status, StatusEscalated)
	}
	events := hitlEvents(hitlProjection(t, env))
	if got := hitlCountEvents(events, journal.EventRunFinished); got != 1 {
		t.Fatalf("run.finished count = %d, want 1 — a deny writes no second terminal: %v", got, hitlEventTypes(events))
	}
	var marker *journal.Event
	for i := range events {
		if events[i].Type == journal.EventRunnerAnnotation &&
			events[i].Runner["kind"] == HITLEscalationResolutionMarker {
			marker = &events[i]
		}
	}
	if marker == nil {
		t.Fatalf("no escalation.resolution annotation: %v", hitlEventTypes(events))
	}
	if marker.Runner["resolution"] != HITLResolutionDeny {
		t.Fatalf("marker resolution = %v, want deny", marker.Runner["resolution"])
	}
	if marker.Runner["actor"] != "ops@example.com" {
		t.Fatalf("marker actor = %v, want the caller identity", marker.Runner["actor"])
	}
	if marker.Runner["idempotencyKey"] != "req-deny" {
		t.Fatalf("marker key = %v, want the request id", marker.Runner["idempotencyKey"])
	}
	if exec.calls["ship"] != 0 {
		t.Fatal("a denied escalation must not dispatch another stage")
	}
}

// TestHITLDuplicateIntentIsIdempotent covers the deduplication contract: a
// second delivery of the SAME request id replays the first ack and resumes
// nothing a second time.
func TestHITLDuplicateIntentIsIdempotent(t *testing.T) {
	exec := hitlTwiceFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResolveEscalation, "req-dupe")
	intent.Gate = "review"
	intent.Resolution = HITLResolutionApprove
	intent.Decision = "pass"

	first := deliver(t, env, time.Minute, "update-1", intent)
	// A genuine client retry: same request id, different Temporal update id
	// (the case server-side dedup does not cover).
	second := deliver(t, env, 2*time.Minute, "update-2", intent)

	env.ExecuteWorkflow(Run, hitlInputFor(t, hitlTwiceEscalatingSpec()))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if first.rejected != nil || first.err != nil {
		t.Fatalf("first delivery refused: rejected=%v err=%v", first.rejected, first.err)
	}
	if second.rejected != nil || second.err != nil {
		t.Fatalf("duplicate delivery refused instead of replayed: rejected=%v err=%v", second.rejected, second.err)
	}
	if !second.ack.Duplicate {
		t.Fatalf("duplicate ack = %+v, want Duplicate=true", second.ack)
	}
	if second.ack.ResumeState != first.ack.ResumeState {
		t.Fatalf("duplicate ack resume state = %q, want the first's %q", second.ack.ResumeState, first.ack.ResumeState)
	}
	events := hitlEvents(hitlProjection(t, env))
	if got := hitlCountEvents(events, journal.EventRunResumed); got != 1 {
		t.Fatalf("run.resumed count = %d, want exactly 1 for a duplicated intent", got)
	}
	if exec.calls["ship"] != 1 {
		t.Fatalf("ship dispatched %d times, want 1 — a duplicate must not resume twice", exec.calls["ship"])
	}
}

// TestHITLRequestIDReusedForDifferentPayloadIsRefused is the other half of
// idempotency: a key reused for a DIFFERENT decision is refused rather than
// silently replaying the first, the same rule the in-process
// escalation-resolution marker enforces.
func TestHITLRequestIDReusedForDifferentPayloadIsRefused(t *testing.T) {
	exec := hitlTwiceFailingExec()
	env := hitlEnv(t, exec)

	first := baseIntent(HITLResolveEscalation, "req-shared")
	first.Gate = "review"
	first.Resolution = HITLResolutionApprove
	first.Decision = "pass"

	second := first
	second.Resolution = HITLResolutionOverride
	second.Rationale = "different decision entirely"

	deliver(t, env, time.Minute, "update-1", first)
	reused := deliver(t, env, 2*time.Minute, "update-2", second)

	env.ExecuteWorkflow(Run, hitlInputFor(t, hitlTwiceEscalatingSpec()))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	refusal := reused.err
	if refusal == nil {
		refusal = reused.rejected
	}
	if refusal == nil {
		t.Fatal("a reused request id with a different payload was accepted")
	}
	if !strings.Contains(refusal.Error(), HITLErrKeyReused) {
		t.Fatalf("refusal = %v, want the %s refusal", refusal, HITLErrKeyReused)
	}
}

// TestHITLIntentWhileExecutingIsRefusedExplicitly pins the phase gate. An
// intent for a run that has not reached a resumable terminal is refused BY
// NAME — never queued — because queueing it would apply an operator's verdict
// to a state they never saw.
func TestHITLIntentWhileExecutingIsRefusedExplicitly(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResolveEscalation, "req-early")
	intent.Gate = "review"
	intent.Resolution = HITLResolutionApprove
	intent.Decision = "pass"
	// Delivered at t=0, before the first stage has even finished.
	early := deliver(t, env, time.Nanosecond, "update-early", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if early.rejected == nil {
		t.Fatal("an intent delivered mid-execution was not rejected")
	}
	if !strings.Contains(early.rejected.Error(), HITLErrRunExecuting) {
		t.Fatalf("rejection = %v, want the %s refusal", early.rejected, HITLErrRunExecuting)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q — a refused early intent must not be applied later", res.Status, StatusEscalated)
	}
	events := hitlEvents(hitlProjection(t, env))
	if got := hitlCountEvents(events, journal.EventRunResumed); got != 0 {
		t.Fatalf("run.resumed count = %d, want 0 — a refused intent must leave no trace", got)
	}
}

// TestHITLUnauthorizedActorIsRefused pins authorization IN THE WORKFLOW: the
// pinned actor set is enforced by the run itself, so a compromised or buggy
// daemon cannot resolve a run its operator was never entitled to.
func TestHITLUnauthorizedActorIsRefused(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResolveEscalation, "req-unauth")
	intent.Gate = "review"
	intent.Resolution = HITLResolutionApprove
	intent.Decision = "pass"
	intent.Actor = "intruder@example.com"
	out := deliver(t, env, time.Minute, "update-unauth", intent)

	env.ExecuteWorkflow(Run, hitlInput(t, "ops@example.com", "sre@example.com"))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected == nil {
		t.Fatal("an unauthorized actor's intent was not rejected")
	}
	if !strings.Contains(out.rejected.Error(), HITLErrUnauthorized) {
		t.Fatalf("rejection = %v, want the %s refusal", out.rejected, HITLErrUnauthorized)
	}
	events := hitlEvents(hitlProjection(t, env))
	if got := hitlCountEvents(events, journal.EventRunResumed); got != 0 {
		t.Fatalf("run.resumed count = %d, want 0", got)
	}
}

// TestHITLContainmentRefusesForeignRunID pins run containment: an intent
// addressed to another run is refused even though Temporal routed it here.
func TestHITLContainmentRefusesForeignRunID(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResumeFromTerminal, "req-foreign")
	intent.RunID = "run-somebody-else"
	intent.Complete = true
	out := deliver(t, env, time.Minute, "update-foreign", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected == nil {
		t.Fatal("an intent addressed to another run was accepted")
	}
	if !strings.Contains(out.rejected.Error(), HITLErrRunMismatch) {
		t.Fatalf("rejection = %v, want the %s refusal", out.rejected, HITLErrRunMismatch)
	}
}

// TestHITLProtocolVersionMismatchIsRefused pins the versioning contract.
func TestHITLProtocolVersionMismatchIsRefused(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResumeFromTerminal, "req-v2")
	intent.Version = HITLProtocolVersion + 1
	intent.Complete = true
	out := deliver(t, env, time.Minute, "update-v2", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if out.rejected == nil {
		t.Fatal("an intent from a future protocol version was accepted")
	}
	if !strings.Contains(out.rejected.Error(), HITLErrProtocol) {
		t.Fatalf("rejection = %v, want the %s refusal", out.rejected, HITLErrProtocol)
	}
}

// TestHITLStaleTerminalGenerationIsRefused is the compare-and-set guard — the
// engine's ExpectedTerminalSeq. An operator resolving against a terminal that
// has since moved is refused, never applied to the new one.
func TestHITLStaleTerminalGenerationIsRefused(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	intent := baseIntent(HITLResolveEscalation, "req-stale")
	intent.Gate = "review"
	intent.Resolution = HITLResolutionApprove
	intent.Decision = "pass"
	intent.ExpectedTerminalGeneration = 7
	out := deliver(t, env, time.Minute, "update-stale", intent)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	refusal := out.err
	if refusal == nil {
		refusal = out.rejected
	}
	if refusal == nil {
		t.Fatal("a stale terminal generation was accepted")
	}
	if !strings.Contains(refusal.Error(), HITLErrGeneration) {
		t.Fatalf("refusal = %v, want the %s refusal", refusal, HITLErrGeneration)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q", res.Status, StatusEscalated)
	}
}

// TestHITLConcurrentIntentsResolveOnce pins the concurrency contract: two
// operators racing on one escalated run produce exactly one resume, and the
// loser is told the generation moved rather than being silently ignored.
func TestHITLConcurrentIntentsResolveOnce(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	approve := baseIntent(HITLResolveEscalation, "req-a")
	approve.Gate = "review"
	approve.Resolution = HITLResolutionApprove
	approve.Decision = "pass"

	rerun := baseIntent(HITLRerunStage, "req-b")
	rerun.Stage = "implement"
	rerun.InstructionAddendum = "try again"

	// Both delivered in the SAME delayed callback, so both updates are
	// admitted against the same workflow state before either handler runs.
	outA := &hitlOutcome{}
	outB := &hitlOutcome{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(HITLUpdateName, "update-a", outA.callbacks(t), approve)
		env.UpdateWorkflow(HITLUpdateName, "update-b", outB.callbacks(t), rerun)
	}, time.Minute)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	resumedCount := 0
	for _, out := range []*hitlOutcome{outA, outB} {
		if out.rejected == nil && out.err == nil && out.ack.Resumed {
			resumedCount++
		}
	}
	if resumedCount != 1 {
		t.Fatalf("%d intents reported a resume, want exactly 1 (a=%+v/%v/%v b=%+v/%v/%v)",
			resumedCount, outA.ack, outA.rejected, outA.err, outB.ack, outB.rejected, outB.err)
	}
	events := hitlEvents(hitlProjection(t, env))
	operatorEvents := hitlCountEvents(events, journal.EventRunResumed) + hitlCountEvents(events, journal.EventStageRerunRequested)
	if operatorEvents != 1 {
		t.Fatalf("operator events = %d, want exactly 1: %v", operatorEvents, hitlEventTypes(events))
	}
}

// TestHITLWindowExpiryLeavesTerminalUnchanged pins the bound on the hold: a
// HITL-enabled run nobody resolves settles with exactly the journal a
// HITL-disabled one would have written.
func TestHITLWindowExpiryLeavesTerminalUnchanged(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)
	in := hitlInput(t)
	in.HITL.WaitSeconds = 60

	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.Status != StatusEscalated {
		t.Fatalf("status = %q, want %q", res.Status, StatusEscalated)
	}
	events := hitlEvents(hitlProjection(t, env))
	if got := hitlCountEvents(events, journal.EventRunFinished); got != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1 after an unresolved hold: %v", got, hitlEventTypes(events))
	}
	if got := hitlCountEvents(events, journal.EventRunResumed); got != 0 {
		t.Fatalf("run.resumed count = %d, want 0", got)
	}
}

// TestHITLCancellationDuringHoldSettlesAborted pins the cancellation
// interaction: `goobers run cancel` on an engine run routes to CancelWorkflow
// (#3877/D2), and a cancellation that lands while a terminal is held open must
// close the run rather than leaving the hold to expire.
func TestHITLCancellationDuringHoldSettlesAborted(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)
	env.ExecuteWorkflow(Run, hitlInput(t))

	if !env.IsWorkflowCompleted() {
		t.Fatal("cancellation did not close a workflow holding a HITL terminal open")
	}
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("a cancelled run must report the cancellation")
	}
	events := hitlEvents(hitlProjection(t, env))
	// The escalated terminal the hold wrote, plus the abort the cancellation
	// wrote on top of it: both facts are true and both are recorded.
	if got := hitlCountEvents(events, journal.EventRunFinished); got != 2 {
		t.Fatalf("run.finished count = %d, want 2 (escalated then aborted): %v", got, hitlEventTypes(events))
	}
	var last journal.Event
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			last = e
		}
	}
	if last.Status != string(journal.PhaseAborted) {
		t.Fatalf("final terminal status = %q, want %q", last.Status, journal.PhaseAborted)
	}
}

// TestHITLStateQueryReportsAcceptingPhase pins the read side an operator UI
// needs: while a terminal is held open the run reports the phase and the
// terminal generation to quote back.
func TestHITLStateQueryReportsAcceptingPhase(t *testing.T) {
	exec := hitlFailingExec()
	env := hitlEnv(t, exec)

	var observed HITLState
	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(HITLStateQuery)
		if err != nil {
			t.Errorf("query HITL state: %v", err)
			return
		}
		if err := val.Get(&observed); err != nil {
			t.Errorf("decode HITL state: %v", err)
		}
	}, time.Minute)

	env.ExecuteWorkflow(Run, hitlInput(t))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if observed.Phase != hitlPhaseAwaiting {
		t.Fatalf("phase = %q, want %q", observed.Phase, hitlPhaseAwaiting)
	}
	if observed.TerminalGeneration != 1 {
		t.Fatalf("terminal generation = %d, want 1", observed.TerminalGeneration)
	}
	if observed.TerminalStatus != StatusEscalated {
		t.Fatalf("terminal status = %q, want %q", observed.TerminalStatus, StatusEscalated)
	}
	if !observed.Enabled {
		t.Fatal("HITL state reported the protocol disabled on an enabled run")
	}
}

// TestHITLRefusesUnresumableTargets pins the shape validation the runner
// applies: a deterministic gate cannot be overridden, a stage that never ran
// cannot be rerun, a deterministic stage takes no addendum, and a reserved
// target is not a resume destination.
func TestHITLRefusesUnresumableTargets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*HITLIntent)
		wantErr string
	}{
		{
			name: "reserved target",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResumeFromTerminal
				in.Target = wf.TargetEscalate
			},
			wantErr: HITLErrNotResumable,
		},
		{
			name: "unknown target",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResumeFromTerminal
				in.Target = "no-such-stage"
			},
			wantErr: HITLErrNotResumable,
		},
		{
			name: "stage that never ran",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLRerunStage
				in.Stage = "ship"
				in.InstructionAddendum = "try harder"
			},
			wantErr: HITLErrNotResumable,
		},
		{
			name: "unknown gate",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResolveEscalation
				in.Gate = "no-such-gate"
				in.Resolution = HITLResolutionApprove
				in.Decision = "pass"
			},
			wantErr: HITLErrInvalidIntent,
		},
		{
			name: "decision with no branch",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResolveEscalation
				in.Gate = "review"
				in.Resolution = HITLResolutionApprove
				in.Decision = "sideways"
			},
			wantErr: HITLErrNotResumable,
		},
		{
			name: "override with no rationale",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResolveEscalation
				in.Gate = "review"
				in.Resolution = HITLResolutionOverride
				in.Decision = "pass"
			},
			wantErr: HITLErrInvalidIntent,
		},
		{
			name: "rerun with no addendum",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLRerunStage
				in.Stage = "implement"
			},
			wantErr: HITLErrInvalidIntent,
		},
		{
			name: "resume with both target and complete",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResumeFromTerminal
				in.Target = "ship"
				in.Complete = true
			},
			wantErr: HITLErrInvalidIntent,
		},
		{
			name: "unknown intent kind",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLIntentKind("delete-everything")
			},
			wantErr: HITLErrInvalidIntent,
		},
		{
			name: "no terminal generation",
			mutate: func(in *HITLIntent) {
				in.Kind = HITLResumeFromTerminal
				in.Complete = true
				in.ExpectedTerminalGeneration = 0
			},
			wantErr: HITLErrInvalidIntent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := hitlFailingExec()
			env := hitlEnv(t, exec)
			intent := baseIntent(HITLResumeFromTerminal, "req-"+tc.name)
			tc.mutate(&intent)
			out := deliver(t, env, time.Minute, "update-"+tc.name, intent)

			env.ExecuteWorkflow(Run, hitlInput(t))
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("workflow error: %v", err)
			}
			refusal := out.rejected
			if refusal == nil {
				refusal = out.err
			}
			if refusal == nil {
				t.Fatalf("intent %+v was accepted; want %s", intent, tc.wantErr)
			}
			if !strings.Contains(refusal.Error(), tc.wantErr) {
				t.Fatalf("refusal = %v, want %s", refusal, tc.wantErr)
			}
			var res RunResult
			if err := env.GetWorkflowResult(&res); err != nil {
				t.Fatalf("workflow result: %v", err)
			}
			if res.Status != StatusEscalated {
				t.Fatalf("status = %q, want %q — a refused intent leaves the terminal alone", res.Status, StatusEscalated)
			}
		})
	}
}

// TestHITLDelivererRequiresAddressableRun covers the daemon-side client: a run
// with no workflow is a not-found, distinct from a refusal.
func TestHITLDelivererRequiresAddressableRun(t *testing.T) {
	if _, err := NewHITLDeliverer(nil); err == nil {
		t.Fatal("NewHITLDeliverer accepted a nil Temporal client")
	}
	var d *HITLDeliverer
	if _, err := d.Deliver(t.Context(), HITLIntent{RunID: "run-x", RequestID: "k"}); err == nil {
		t.Fatal("a nil deliverer accepted an intent")
	}
}

// TestHITLRefusalCodeReadsProtocolCode pins that a refusal's stable code
// survives the round trip a daemon does to map it onto an HTTP status.
func TestHITLRefusalCodeReadsProtocolCode(t *testing.T) {
	err := hitlRefusal(HITLErrRunSettled, "run %s has settled", "run-x")
	code, message, ok := HITLRefusalCode(err)
	if !ok || code != HITLErrRunSettled {
		t.Fatalf("HITLRefusalCode = (%q, %v), want (%q, true)", code, ok, HITLErrRunSettled)
	}
	if message != "run run-x has settled" {
		t.Fatalf("refusal message = %q, want the workflow's own sentence", message)
	}
	if _, _, ok := HITLRefusalCode(errors.New("plain")); ok {
		t.Fatal("a plain error was reported as a protocol refusal")
	}
	// An application error that is NOT one of this protocol's refusals must
	// not be laundered into an operator-facing 4xx.
	other := temporal.NewNonRetryableApplicationError("journal write failed", "journal_write_failed", nil)
	if _, _, ok := HITLRefusalCode(other); ok {
		t.Fatal("a non-protocol application error was reported as a protocol refusal")
	}
}
