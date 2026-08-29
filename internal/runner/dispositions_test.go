package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

// TestIssueNotApplicableIsATerminalDeliverable is #3363's regression, driven
// through a real Start() over the observed live shape (run ec3cd05e): the
// implement stage is handed a stale issue, verifies the premise no longer
// holds, and returns status:failure ISSUE_NOT_APPLICABLE with its citation as
// the summary.
//
// Before the fix, that refusal routed into the Next gate: the reviewer
// evaluated an empty diff, review-failed it, the repass loop re-derived the
// identical conclusion until the budget exhausted, and the reasoning never
// left events.jsonl. After it, the refusal is a terminal disposition on the
// FIRST attempt — the reviewer is never invoked — and the stage's own
// reasoning posts to the driving issue as an escalation comment, which is the
// run's actual deliverable.
func TestIssueNotApplicableIsATerminalDeliverable(t *testing.T) {
	const runID = "run-not-applicable"
	const citation = "The issue targets portal/src/auth/msal.ts, intentionally removed by #3122; " +
		"restoring it would reintroduce unused production code."

	reviewer := &capturingReviewer{}
	commenter := &recordingCommenter{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":implement": {
					status:  apiv1.ResultFailure,
					summary: citation,
					errorInfo: &apiv1.ErrorInfo{
						Code:      telemetry.ErrCodeIssueNotApplicable,
						Message:   "the issue's premise no longer holds",
						Retryable: false,
					},
				},
			}}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Escalation:   &gate.EscalationNotifier{Poster: commenter},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: agenticGateMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "2442", Provider: apiv1.ProviderGitHub, Title: "stale ticket"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated (a verified refusal is terminal on attempt 1, never a repass loop)", res.Phase)
	}
	if reviewer.called {
		t.Fatal("the reviewer was invoked — ISSUE_NOT_APPLICABLE must bypass the gate and its repass loop entirely")
	}
	if len(commenter.requests) != 1 {
		t.Fatalf("issue comments = %d, want exactly 1 (the refusal's reasoning is the deliverable)", len(commenter.requests))
	}
	got := commenter.requests[0]
	if got.ID != "2442" {
		t.Fatalf("comment posted to item %q, want the driving item 2442", got.ID)
	}
	if !strings.Contains(got.Comment, citation) {
		t.Fatalf("comment = %q, want it to carry the stage's own citation %q", got.Comment, citation)
	}
	if !strings.Contains(got.Comment, "implement") {
		t.Fatalf("comment = %q, want the refusing stage named", got.Comment)
	}

	// The refusal must also be classifiable from the journal alone, so the
	// rollup can split it out of the success metric (#3364) without parsing
	// message text.
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawCodedFinish bool
	for _, event := range events {
		if event.Type != journal.EventStageFinished || event.Stage != "implement" || event.Error == nil {
			continue
		}
		if event.Error.Code != telemetry.ErrCodeIssueNotApplicable {
			t.Fatalf("stage.finished error code = %q, want %q", event.Error.Code, telemetry.ErrCodeIssueNotApplicable)
		}
		if event.Error.Message != citation {
			t.Fatalf("stage.finished message = %q, want the refusal citation", event.Error.Message)
		}
		sawCodedFinish = true
	}
	if !sawCodedFinish {
		t.Fatal("no stage.finished event carried the ISSUE_NOT_APPLICABLE disposition")
	}
	if class := telemetry.ClassifyError(telemetry.ErrCodeIssueNotApplicable); class != telemetry.ErrorClassItemJudgment {
		t.Fatalf("ISSUE_NOT_APPLICABLE classifies as %q, want %q", class, telemetry.ErrorClassItemJudgment)
	}
}

// credentialFaultingDeterministic fails every dispatch with a
// credential-materialization fault typed and marked exactly as
// internal/executor and internal/harness now mark it (#3361), counting how
// many attempts the runner spent on it.
type credentialFaultingDeterministic struct {
	attempts int
}

func (d *credentialFaultingDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.attempts++
	return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(
		codedStageFailure(telemetry.ErrCodeCredentialUnavailable,
			errCredentialMaterialization),
	)
}

var errCredentialMaterialization = &credentialMaterializationError{}

type credentialMaterializationError struct{}

func (*credentialMaterializationError) Error() string {
	return "harness: materialize credentials: GET /user: 403"
}

// TestCredentialFaultDoesNotConsumeAgenticAttempts is #3361's regression: the
// live shape was a credential-materialization failure (a GET /user 403)
// recorded as `error … (attempt 1/1)` — the infrastructure fault consumed the
// stage's ONLY policy attempt and terminated the run as a work failure.
//
// After the fix the fault flows through the bounded INFRASTRUCTURE budget
// instead: every retry after the first is journaled AttemptClass "infra"
// (conformance-excluded, excluded from the policy budget), the run dispatches
// DefaultMaxInfrastructureAttempts times rather than once, and the terminal
// carries the typed credential_unavailable code so downstream disposition
// consumers (the failure-streak breaker, the success metric) can tell weather
// from work.
func TestCredentialFaultDoesNotConsumeAgenticAttempts(t *testing.T) {
	const runID = "run-credential-fault"
	det := &credentialFaultingDeterministic{}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return det, nil
	}, nil)

	// A single-attempt policy budget — the exact configuration under which the
	// old classification converted transient infrastructure weather into a
	// permanent-looking work failure.
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: retryFixtureMachine(t, 1),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Start: want the exhausted-infrastructure-budget error")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed after the infrastructure budget exhausts", res.Phase)
	}
	if det.attempts != int(DefaultMaxInfrastructureAttempts) {
		t.Fatalf("dispatches = %d, want %d (the infrastructure budget, NOT the 1-attempt policy budget)",
			det.attempts, DefaultMaxInfrastructureAttempts)
	}
	if res.FailureCode != telemetry.ErrCodeCredentialUnavailable {
		t.Fatalf("terminal FailureCode = %q, want %q so the disposition is machine-readable",
			res.FailureCode, telemetry.ErrCodeCredentialUnavailable)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var infraAttempts, policyAttempts int
	var sawTypedClass, sawTerminalClass bool
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			if event.Attempt > 1 {
				if event.AttemptClass == journal.AttemptInfra {
					infraAttempts++
				} else {
					policyAttempts++
				}
			}
		case journal.EventError:
			if event.Runner == nil {
				continue
			}
			if event.Runner[stageErrorCodeKey] == telemetry.ErrCodeCredentialUnavailable &&
				event.Runner[stageErrorClassKey] == string(telemetry.ErrorClassInfra) {
				sawTypedClass = true
			}
			if event.Error != nil && event.Error.Code == "run_failed" &&
				event.Runner[stageErrorClassKey] == string(telemetry.ErrorClassInfra) {
				sawTerminalClass = true
			}
		}
	}
	if policyAttempts != 0 {
		t.Fatalf("policy-class retries = %d, want 0 — an infrastructure fault must never charge the work-attempt budget", policyAttempts)
	}
	if infraAttempts != int(DefaultMaxInfrastructureAttempts)-1 {
		t.Fatalf("infra-class retries = %d, want %d", infraAttempts, DefaultMaxInfrastructureAttempts-1)
	}
	if !sawTypedClass {
		t.Fatal("no attempt error carried errorCode=credential_unavailable / errorClass=infra in the runner namespace")
	}
	if !sawTerminalClass {
		t.Fatal("the terminal run_failed cause carried no infra classification — the rollup cannot split it from a work failure")
	}
	if !telemetry.ClassifyError(telemetry.ErrCodeCredentialUnavailable).InfraFault() {
		t.Fatal("credential_unavailable must classify as an infrastructure fault")
	}
}
