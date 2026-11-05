package runner

import (
	"context"
	"fmt"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestAgenticFactoryReceivesCompilerMaterializedMCPServers(t *testing.T) {
	for _, tc := range []struct {
		version      string
		wantRequired bool
	}{
		{version: "3.0"},
		{version: "3.1", wantRequired: true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web", Start: "work", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
				Tasks: []apiv1.Task{{Name: "work", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "work"}},
			}
			machine, err := workflow.Compile(
				workflow.Definition{Name: "work", DSLVersion: tc.version, Spec: spec},
				workflow.WithPreviewFeatures(true),
				workflow.WithGoobers(map[string]apiv1.GooberSpec{
					"coder": {MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}}},
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			var captured []apiv1.MCPServer
			ex := newExecutors(Config{
				NewAgenticWithMCPServers: func(_ string, servers []apiv1.MCPServer, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
					captured = servers
					active := true
					return &guardedAssetGoober{guardActive: &active}, nil
				},
			}, nil, nil)
			if _, err := ex.agentic("coder", machine); err != nil {
				t.Fatal(err)
			}
			if len(captured) != 1 {
				t.Fatalf("servers = %+v", captured)
			}
			if tc.wantRequired {
				if captured[0].Required == nil || !*captured[0].Required {
					t.Fatalf("DSL 3.1 required = %v, want true", captured[0].Required)
				}
			} else if captured[0].Required != nil {
				t.Fatalf("DSL 3.0 required = %v, want legacy nil", *captured[0].Required)
			}
		})
	}
}

type guardedAssetGoober struct {
	guardActive *bool
}

func (*guardedAssetGoober) HasAssetBundle() bool {
	return true
}

func (g *guardedAssetGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if !*g.guardActive {
		return apiv1.ResultEnvelope{}, fmt.Errorf("asset path guard was not active before invocation")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *guardedAssetGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if !*g.guardActive {
		return apiv1.Verdict{}, fmt.Errorf("asset path guard was not active before review")
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestGooberInvocationActivatesAssetPathGuardBeforeCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*gooberInvocation) error
	}{
		{
			name: "invoke",
			call: func(g *gooberInvocation) error {
				_, err := g.Invoke(context.Background(), apiv1.InvocationEnvelope{})
				return err
			},
		},
		{
			name: "review",
			call: func(g *gooberInvocation) error {
				_, err := g.Review(context.Background(), apiv1.InvocationEnvelope{})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guardActive := false
			invocation := &gooberInvocation{
				Goober: &guardedAssetGoober{guardActive: &guardActive},
				activateAssetPathGuard: func() error {
					guardActive = true
					return nil
				},
			}

			if err := tc.call(invocation); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !invocation.materializedAssets() {
				t.Fatalf("%s did not record materialized assets", tc.name)
			}
		})
	}
}

func TestOperatorMessageAddressForAttemptUsesStageStartSequence(t *testing.T) {
	const runID = "run-operator-address"
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID: runID, Workflow: "workflow", WorkflowVersion: 1,
		Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	startedSeq := run.Seq()

	got := operatorMessageAddressForAttempt(run, runID, "implement", 1, "coder")
	want, err := journal.StageAgentAddress(runID, "implement", 1, "coder", startedSeq)
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Fatalf("operatorMessageAddressForAttempt = %q, want %q", got, want.String())
	}
}

func TestOperatorMessageAddressForAttemptUsesGateStartSequence(t *testing.T) {
	const runID = "run-gate-operator-address"
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID: runID, Workflow: "workflow", WorkflowVersion: 1,
		Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{
		Type: journal.EventGateStarted, Gate: "review",
		Runner: map[string]any{"repassAttempt": 2},
	}); err != nil {
		t.Fatal(err)
	}
	startedSeq := run.Seq()

	got := operatorMessageAddressForAttempt(run, runID, "review", 2, "reviewer")
	want, err := journal.StageAgentAddress(runID, "review", 2, "reviewer", startedSeq)
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Fatalf("operatorMessageAddressForAttempt = %q, want %q", got, want.String())
	}
}

type gateHeartbeatJournalStub struct{}

func (gateHeartbeatJournalStub) Append(journal.Event) error  { return nil }
func (gateHeartbeatJournalStub) ObserveActivity()            {}
func (gateHeartbeatJournalStub) RepairAppendBoundary() error { return nil }

type attemptCapturingGoober struct {
	attempt int
}

func (g *attemptCapturingGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *attemptCapturingGoober) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.attempt = int(env.Attempt)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestGateHeartbeatGooberPropagatesAttempt(t *testing.T) {
	capturing := &attemptCapturingGoober{}
	r := &Runner{
		heartbeatInterval: StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker {
			return &fakeHeartbeatTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
		},
	}
	goober := gateHeartbeatGoober{
		goober: capturing, runner: r, journal: gateHeartbeatJournalStub{},
		stage: "review", attempt: 3,
	}
	if _, err := goober.Review(context.Background(), apiv1.InvocationEnvelope{}); err != nil {
		t.Fatal(err)
	}
	if capturing.attempt != 3 {
		t.Fatalf("review attempt = %d, want 3", capturing.attempt)
	}
}

func TestGateHeartbeatGooberComposesWithAssetGuard(t *testing.T) {
	guardActive := false
	invocation := &gooberInvocation{
		Goober: &guardedAssetGoober{guardActive: &guardActive},
		activateAssetPathGuard: func() error {
			guardActive = true
			return nil
		},
	}
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	r := &Runner{
		heartbeatInterval:  StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker { return ticker },
	}
	goober := gateHeartbeatGoober{
		goober: invocation, runner: r, journal: gateHeartbeatJournalStub{},
		stage: "review", attempt: 1,
	}

	verdict, err := goober.Review(context.Background(), apiv1.InvocationEnvelope{})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("decision = %q, want pass", verdict.Decision)
	}
	if !invocation.materializedAssets() {
		t.Fatal("asset wrapper was not invoked through the heartbeat wrapper")
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("heartbeat ticker was not stopped after review")
	}
}

func TestReviewerOperatorMessageTargetRebindsEachDispatch(t *testing.T) {
	run := newRunnerTestJournal(t, "reviewer-addresses")
	capturing := &attemptCapturingGoober{}
	invocation := newGooberInvocation(capturing, nil, run, "reviewer-addresses", "review", 1, "reviewer")
	var previous string
	for _, number := range []int{1, 2, 1} {
		if err := run.Append(journal.ReviewerAttemptEvent(journal.EventReviewerStarted, "review", number, "")); err != nil {
			t.Fatal(err)
		}
		start := run.Seq()
		// An intervening record must not become the attempt's address anchor.
		if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "review", Attempt: number}); err != nil {
			t.Fatal(err)
		}
		if _, err := invocation.Review(context.Background(), apiv1.InvocationEnvelope{Attempt: int32(number)}); err != nil {
			t.Fatal(err)
		}
		want, err := journal.StageAgentAddress("reviewer-addresses", "review", number, "reviewer", start)
		if err != nil {
			t.Fatal(err)
		}
		if invocation.operatorMessageAddress != want.String() || invocation.operatorMessageAddress == previous {
			t.Fatalf("dispatch target=%q want=%q previous=%q", invocation.operatorMessageAddress, want.String(), previous)
		}
		previous = invocation.operatorMessageAddress
	}
}
