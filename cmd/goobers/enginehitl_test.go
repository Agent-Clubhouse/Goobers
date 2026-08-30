package main

// enginehitl_test.go is the daemon half of #3883's acceptance surface.
//
// The engine half (internal/engine/hitl_test.go, hitlreplay_test.go) proves
// the workflow decides correctly. This half proves the daemon reaches it: that
// an engine-driven run's intervention is TRANSLATED and DELIVERED rather than
// refused, that a runner-driven run's is not touched, and — the issue's
// closing clause — that no in-process runner path is invoked for an engine
// run.
//
// The runner double is the load-bearing part. The intervention service is
// constructed with a real runner.Runner over a real run directory, so if any
// verb fell through to Resume/ResumeFromTerminal/RerunStage the deterministic
// executor would be invoked and the run's journal would grow the runner's own
// events. Both are asserted against.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// recordingDeliverer stands in for the Temporal update client. It records what
// the daemon translated and answers with whatever the test asks it to.
type recordingDeliverer struct {
	mu       sync.Mutex
	intents  []engine.HITLIntent
	ack      engine.HITLAck
	err      error
	delivers int
}

func (d *recordingDeliverer) Deliver(_ context.Context, intent engine.HITLIntent) (engine.HITLAck, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivers++
	d.intents = append(d.intents, intent)
	if d.err != nil {
		return engine.HITLAck{}, d.err
	}
	return d.ack, nil
}

func (d *recordingDeliverer) last(t *testing.T) engine.HITLIntent {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.intents) == 0 {
		t.Fatal("no operator intent was delivered to the engine")
	}
	return d.intents[len(d.intents)-1]
}

// refusingDeterministic fails the test if the in-process runner ever dispatches
// a stage. It is what turns "the runner was not invoked" from an assertion
// about journal side effects into an assertion about the call itself.
type refusingDeterministic struct{ t *testing.T }

func (d refusingDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.t.Error("the in-process runner dispatched a stage for an engine-driven run")
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// engineHITLFixture builds an escalated, engine-driven run with a deliverer
// attached and a runner that fails the test if it is used.
func engineHITLFixture(t *testing.T, runID string, deliverer hitlDeliverer) (*runInterventionService, string) {
	t.Helper()
	machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRunWithDeterministic(t, machine, runID, []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, refusingDeterministic{t: t})
	markRunYAMLEngineDriven(t, runDir)
	service.AttachHITLDeliverer(deliverer)
	return service, runDir
}

// journalDigest is the run's whole event log as written on disk. An engine
// run's journal is authored by the workflow, never by this process, so a
// daemon-side intervention must leave these bytes untouched.
func journalDigest(t *testing.T, runDir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read run journal: %v", err)
	}
	return raw
}

// engineHITLRefusal builds the kind of refusal the workflow returns: a
// non-retryable Temporal application error whose TYPE is the protocol code.
func engineHITLRefusal(code, message string) error {
	return temporal.NewNonRetryableApplicationError(message, code, nil)
}

// TestEngineHITLApproveDeliversIntentAndTouchesNoRunner is the far-side
// evidence's daemon clause: an operator approves an escalated engine-driven
// run, the intent reaches the workflow carrying the operator's identity and
// the terminal generation they saw, and no in-process runner path runs.
func TestEngineHITLApproveDeliversIntentAndTouchesNoRunner(t *testing.T) {
	deliverer := &recordingDeliverer{ack: engine.HITLAck{Resumed: true, ResumeState: "implement"}}
	const runID = "engine-hitl-approve"
	service, runDir := engineHITLFixture(t, runID, deliverer)
	before := journalDigest(t, runDir)

	if _, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "ops@example.com", Decision: "pass",
		IdempotencyKey: "key-approve-1",
	}); err != nil {
		t.Fatalf("approve on an engine-driven run: %v", err)
	}

	intent := deliverer.last(t)
	if intent.Protocol != engine.HITLProtocol || intent.Version != engine.HITLProtocolVersion {
		t.Fatalf("intent protocol = %s/%d, want %s/%d",
			intent.Protocol, intent.Version, engine.HITLProtocol, engine.HITLProtocolVersion)
	}
	if intent.Kind != engine.HITLResolveEscalation || intent.Resolution != engine.HITLResolutionApprove {
		t.Fatalf("intent = %+v, want a resolve-escalation approve", intent)
	}
	if intent.RunID != runID {
		t.Fatalf("intent run = %q, want %q — an intent must be contained to the run it was issued for", intent.RunID, runID)
	}
	if intent.Actor != "ops@example.com" {
		t.Fatalf("intent actor = %q, want the operator's identity preserved", intent.Actor)
	}
	if intent.Gate != "review" || intent.Decision != "pass" {
		t.Fatalf("intent = %+v, want gate review decision pass", intent)
	}
	if intent.RequestID != "key-approve-1" {
		t.Fatalf("intent request id = %q, want the Idempotency-Key verbatim", intent.RequestID)
	}
	if intent.ExpectedTerminalGeneration != 1 {
		t.Fatalf("intent generation = %d, want 1 — the run has produced exactly one terminal",
			intent.ExpectedTerminalGeneration)
	}
	if after := journalDigest(t, runDir); string(after) != string(before) {
		t.Fatal("the daemon wrote to an engine-driven run's journal; only the workflow may author it")
	}
}

// TestEngineHITLVerbsTranslateToTheirIntents pins every verb's mapping. A verb
// silently translating to the wrong intent would resolve an operator's
// escalation as something they did not ask for.
func TestEngineHITLVerbsTranslateToTheirIntents(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		call       func(*runInterventionService, httpapi.InterventionRequest) error
		input      httpapi.InterventionRequest
		kind       engine.HITLIntentKind
		resolution string
		verify     func(*testing.T, engine.HITLIntent)
	}{
		{
			name: "approve",
			call: func(s *runInterventionService, in httpapi.InterventionRequest) error {
				_, err := s.Approve(ctx, in)
				return err
			},
			input:      httpapi.InterventionRequest{Stage: "review", Actor: "ops", Decision: "pass"},
			kind:       engine.HITLResolveEscalation,
			resolution: engine.HITLResolutionApprove,
		},
		{
			name: "override",
			call: func(s *runInterventionService, in httpapi.InterventionRequest) error {
				_, err := s.Override(ctx, in)
				return err
			},
			input: httpapi.InterventionRequest{
				Stage: "review", Actor: "ops", Decision: "pass", Rationale: "reviewed by hand",
			},
			kind:       engine.HITLResolveEscalation,
			resolution: engine.HITLResolutionOverride,
			verify: func(t *testing.T, intent engine.HITLIntent) {
				if intent.Rationale != "reviewed by hand" {
					t.Fatalf("override rationale = %q, want the operator's", intent.Rationale)
				}
			},
		},
		{
			name: "rerun",
			call: func(s *runInterventionService, in httpapi.InterventionRequest) error {
				_, err := s.RerunStage(ctx, in)
				return err
			},
			input: httpapi.InterventionRequest{
				Stage: "implement", Actor: "ops", InstructionAddendum: "add the missing test",
			},
			kind: engine.HITLRerunStage,
			verify: func(t *testing.T, intent engine.HITLIntent) {
				if intent.Stage != "implement" {
					t.Fatalf("rerun stage = %q, want implement", intent.Stage)
				}
				if intent.InstructionAddendum != "add the missing test" {
					t.Fatalf("rerun addendum = %q, want the operator's", intent.InstructionAddendum)
				}
			},
		},
		{
			name: "deny",
			call: func(s *runInterventionService, in httpapi.InterventionRequest) error {
				_, err := s.AcceptDenyEscalation(ctx, ctx, in)
				return err
			},
			input: httpapi.InterventionRequest{
				Actor: "ops", Rationale: "the escalation stands", IdempotencyKey: "key-deny",
			},
			kind:       engine.HITLResolveEscalation,
			resolution: engine.HITLResolutionDeny,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deliverer := &recordingDeliverer{ack: engine.HITLAck{}}
			runID := "engine-hitl-" + tc.name
			service, _ := engineHITLFixture(t, runID, deliverer)
			input := tc.input
			input.RunID = runID
			if err := tc.call(service, input); err != nil {
				t.Fatalf("%s on an engine-driven run: %v", tc.name, err)
			}
			intent := deliverer.last(t)
			if intent.Kind != tc.kind {
				t.Fatalf("intent kind = %q, want %q", intent.Kind, tc.kind)
			}
			if tc.resolution != "" && intent.Resolution != tc.resolution {
				t.Fatalf("intent resolution = %q, want %q", intent.Resolution, tc.resolution)
			}
			if intent.RequestID == "" {
				t.Fatal("intent carries no request id; the protocol cannot deduplicate it")
			}
			if tc.verify != nil {
				tc.verify(t, intent)
			}
		})
	}
}

// TestEngineHITLWithoutDelivererKeepsTheRefusal is the rollback proof at the
// daemon layer: a type-1/type-2 install, or any daemon with no engine client,
// answers exactly as it did before #3883.
func TestEngineHITLWithoutDelivererKeepsTheRefusal(t *testing.T) {
	const runID = "engine-hitl-no-deliverer"
	machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, runID, []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	markRunYAMLEngineDriven(t, runDir)

	_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "ops", Decision: "pass",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("approve error = %v (%T), want an httpapi.InterventionError", err, err)
	}
	if interventionErr.Code != "run_engine_driven" {
		t.Fatalf("approve error code = %q, want the pre-#3883 run_engine_driven refusal", interventionErr.Code)
	}
}

// TestEngineHITLRefusalsMapToTheirStatuses pins the protocol-to-HTTP contract.
// An operator's client branches on these codes, so a refusal arriving as a 500
// — or as the wrong 4xx — is a behavioural regression even though the run is
// unharmed.
func TestEngineHITLRefusalsMapToTheirStatuses(t *testing.T) {
	cases := []struct {
		code   string
		status int
		want   string
	}{
		{engine.HITLErrUnauthorized, 403, "intervention_forbidden"},
		{engine.HITLErrInvalidIntent, 400, "invalid_intervention"},
		{engine.HITLErrProtocol, 400, "protocol_unsupported"},
		{engine.HITLErrKeyReused, 409, "idempotency_key_reused"},
		{engine.HITLErrGeneration, 409, "terminal_generation_changed"},
		{engine.HITLErrRunExecuting, 409, "run_not_intervenable"},
		{engine.HITLErrRunSettled, 409, "run_not_intervenable"},
		{engine.HITLErrNotResumable, 409, "gate_not_approvable"},
		{engine.HITLErrRunMismatch, 409, "run_identity_mismatch"},
		{engine.HITLErrNotAcceptingUp, 409, "run_engine_driven"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			deliverer := &recordingDeliverer{err: engineHITLRefusal(tc.code, "the run said no")}
			runID := "engine-hitl-refusal"
			service, _ := engineHITLFixture(t, runID, deliverer)

			_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
				RunID: runID, Stage: "review", Actor: "ops", Decision: "pass",
			})
			var interventionErr *httpapi.InterventionError
			if !errors.As(err, &interventionErr) {
				t.Fatalf("error = %v (%T), want an httpapi.InterventionError", err, err)
			}
			if interventionErr.Status != tc.status {
				t.Fatalf("status = %d, want %d", interventionErr.Status, tc.status)
			}
			if interventionErr.Code != tc.want {
				t.Fatalf("code = %q, want %q", interventionErr.Code, tc.want)
			}
			if !strings.Contains(interventionErr.Error(), "the run said no") {
				t.Fatalf("error = %q, want the workflow's own sentence preserved", interventionErr.Error())
			}
		})
	}
}

// TestEngineHITLUnknownWorkflowIsNotFound: an intent for a run whose workflow
// is gone is a 404, not a refusal. The two mean different things to an
// operator — "there is nothing to talk to" versus "it heard you and said no".
func TestEngineHITLUnknownWorkflowIsNotFound(t *testing.T) {
	deliverer := &recordingDeliverer{err: engine.ErrHITLRunNotFound}
	const runID = "engine-hitl-missing"
	service, _ := engineHITLFixture(t, runID, deliverer)

	_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "ops", Decision: "pass",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("error = %v (%T), want an httpapi.InterventionError", err, err)
	}
	if interventionErr.Status != 404 || interventionErr.Code != "run_not_found" {
		t.Fatalf("error = %d/%s, want 404/run_not_found", interventionErr.Status, interventionErr.Code)
	}
}

// TestEngineHITLNonProtocolErrorIsNotLaundered: an error that is NOT one of
// the protocol's considered refusals must surface as a 500. Reporting a
// transport failure as a 409 would tell an operator the run refused them when
// the run never heard them.
func TestEngineHITLNonProtocolErrorIsNotLaundered(t *testing.T) {
	deliverer := &recordingDeliverer{err: errors.New("connection reset")}
	const runID = "engine-hitl-transport"
	service, _ := engineHITLFixture(t, runID, deliverer)

	_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "ops", Decision: "pass",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("error = %v (%T), want an httpapi.InterventionError", err, err)
	}
	if interventionErr.Status != 500 {
		t.Fatalf("status = %d, want 500 for a transport failure", interventionErr.Status)
	}
}

// TestEngineHITLRequestIDFallsBackToTheFingerprint: a client that sent no
// Idempotency-Key still gets deduplication, because an identical retry
// fingerprints identically. Two DIFFERENT decisions must not collide.
func TestEngineHITLRequestIDFallsBackToTheFingerprint(t *testing.T) {
	deliverer := &recordingDeliverer{}
	const runID = "engine-hitl-fingerprint"
	service, _ := engineHITLFixture(t, runID, deliverer)
	ctx := context.Background()

	base := httpapi.InterventionRequest{RunID: runID, Stage: "review", Actor: "ops", Decision: "pass"}
	if _, err := service.Approve(ctx, base); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := service.Approve(ctx, base); err != nil {
		t.Fatalf("retried approve: %v", err)
	}
	different := base
	different.Decision = "fail"
	if _, err := service.Approve(ctx, different); err != nil {
		t.Fatalf("approve with a different decision: %v", err)
	}

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if len(deliverer.intents) != 3 {
		t.Fatalf("delivered %d intents, want 3", len(deliverer.intents))
	}
	if deliverer.intents[0].RequestID != deliverer.intents[1].RequestID {
		t.Fatal("an identical retry got a different request id; the protocol could not deduplicate it")
	}
	if deliverer.intents[0].RequestID == deliverer.intents[2].RequestID {
		t.Fatal("a different decision reused the first's request id; it would replay the wrong answer")
	}
}

// TestEngineHITLLeavesRunnerDrivenRunsAlone is the containment test in the
// other direction. A runner-driven run must take the in-process path even on a
// daemon with a deliverer attached, or #3883 would have quietly moved every
// intervention in the product onto Temporal.
func TestEngineHITLLeavesRunnerDrivenRunsAlone(t *testing.T) {
	deliverer := &recordingDeliverer{}
	const runID = "runner-run-intervention"
	machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, runID, []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	service.AttachHITLDeliverer(deliverer)

	// The outcome of the runner path is not this test's subject; that it was
	// the path TAKEN is. A runner-driven run must never reach the deliverer.
	_, _ = service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "ops", Decision: "pass",
	})

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if deliverer.delivers != 0 {
		t.Fatalf("a runner-driven run delivered %d operator intents to the engine, want 0", deliverer.delivers)
	}
}

// TestTerminalGenerationCountsTerminals pins the compare-and-set token the
// daemon quotes. It must be the number of terminals — the same number the
// workflow counts — and not a sequence number or a boolean.
func TestTerminalGenerationCountsTerminals(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventRunStarted},
		{Type: journal.EventStageStarted, Stage: "implement"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{Type: journal.EventRunResumed, Status: string(journal.PhaseEscalated)},
		{Type: journal.EventStageStarted, Stage: "implement"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}
	if got := terminalGeneration(events); got != 2 {
		t.Fatalf("terminalGeneration = %d, want 2", got)
	}
	if got := terminalGeneration(nil); got != 0 {
		t.Fatalf("terminalGeneration on a run with no terminal = %d, want 0", got)
	}
}

// TestEngineHITLPolicyIsOptIn pins the rollback posture at its source: an
// instance that did not configure engine.hitl pins NO policy, which is
// byte-identical to every run started before the protocol existed.
func TestEngineHITLPolicyIsOptIn(t *testing.T) {
	if policy := engineHITLPolicy(&instance.Config{}); policy != nil {
		t.Fatalf("policy = %+v on an instance with no engine.hitl block, want nil", policy)
	}
	off := &instance.Config{Engine: &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q",
		HITL: &instance.EngineHITLConfig{Enabled: false, Window: "4h"},
	}}
	if policy := engineHITLPolicy(off); policy != nil {
		t.Fatalf("policy = %+v on a disabled engine.hitl block, want nil", policy)
	}
	on := &instance.Config{Engine: &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q",
		HITL: &instance.EngineHITLConfig{Enabled: true, Window: "4h", Actors: []string{"ops"}},
	}}
	policy := engineHITLPolicy(on)
	if policy == nil || !policy.Enabled {
		t.Fatalf("policy = %+v on an enabled engine.hitl block, want an enabled policy", policy)
	}
	if policy.WaitSeconds != 4*60*60 {
		t.Fatalf("policy window = %ds, want 14400", policy.WaitSeconds)
	}
	if len(policy.Actors) != 1 || policy.Actors[0] != "ops" {
		t.Fatalf("policy actors = %v, want the configured set", policy.Actors)
	}
	// An unbounded window is refused at load rather than silently defaulting.
	bad := instance.EngineHITLConfig{Enabled: true, Window: "4hr"}
	if err := bad.Validate(); err == nil {
		t.Fatal("an unparsable hold window was accepted")
	}
	if err := (instance.EngineHITLConfig{Enabled: true, Window: "-1h"}).Validate(); err == nil {
		t.Fatal("a negative hold window was accepted")
	}
}
