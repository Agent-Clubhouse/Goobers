package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// This file is a fixture-driven dry run of the shipped implementation
// workflow (issue #27, config-examples/gaggles/acme-web/workflows/
// implementation.yaml): it loads the real definition and walks it through
// the compiled machine with fakes standing in for the not-yet-built
// backlog-query/open-pr/ci-poll stage kinds (#18), the Copilot CLI harness
// (#19), and the local runner (#17) — including the runner-contract
// convention (documented in internal/gate) of flattening a subject stage's
// status/outputs into an automated gate's Inputs, which this superseded
// Temporal adapter predates and does not itself implement. These fakes
// therefore track fixture state directly (closures) rather than routing it
// through env.Inputs, to prove the DSL's loop *shape* is walkable; the real
// Inputs-flattening + bounded-repass-budget + journaling (internal/gate,
// #20) wires into #17's local runner, not this adapter.

const implementationConfigRoot = "../../config-examples/gaggles/acme-web"

func loadImplementationWorkflow(t *testing.T) apiv1.WorkflowSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(implementationConfigRoot, "workflows", "implementation.yaml"))
	if err != nil {
		t.Fatalf("read implementation.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return w.Spec
}

func implementationRunInput(spec apiv1.WorkflowSpec) RunInput {
	return RunInput{
		RunID:                  "run-implementation",
		Gaggle:                 "acme-web",
		WorkflowName:           "implementation",
		Version:                1,
		PreviewFeaturesEnabled: true,
		Spec:                   spec,
		RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}
}

// fixtureAuto dispatches automated-gate checks by AutomatedGate.Check against
// externally-owned fixture state (see file doc comment for why: the
// superseded adapter doesn't flatten a subject stage's result into the
// gate's Inputs, so a real internal/gate-style checker has nothing to read).
type fixtureAuto struct {
	mu        sync.Mutex
	localOK   bool
	ciStatus  string // "", "failing", "passing"
	localGate int
	ciGate    int
}

func (f *fixtureAuto) Evaluate(_ context.Context, gate apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch gate.Check {
	case "status-equals":
		f.localGate++
		if f.localOK {
			return "pass", nil
		}
		return "fail", nil
	case "ci-status":
		f.ciGate++
		if f.ciStatus == "passing" {
			return "pass", nil
		}
		return "fail", nil
	case "output-equals":
		// #947: open-pr-gate (opened=true -> ci-poll, opened=false -> @abort).
		// These fixtures exercise the still-open-issue happy path, so open-pr
		// opened its PR and the gate passes through to ci-poll.
		return "pass", nil
	default:
		return "", nil
	}
}

// TestImplementationDryRunCIFailThenPass exercises issue #27's headline
// acceptance scenario: issue -> PR -> CI fail -> repass -> CI pass ->
// complete. The reviewer and local-ci gates always pass so the CI loop is
// isolated; ci-poll fails on its first call and passes on its second, driven
// by the shared fixtureAuto/counters closures.
func TestImplementationDryRunCIFailThenPass(t *testing.T) {
	spec := loadImplementationWorkflow(t)

	auto := &fixtureAuto{localOK: true, ciStatus: "failing"}
	var mu sync.Mutex
	implementCalls, ciPollCalls, closeOutCalls := 0, 0, 0

	det := &fakeRunner{
		run: func(_ context.Context, env apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(r.Command) == 0 {
				return apiv1.ResultEnvelope{}, nil
			}
			switch r.Command[1] {
			case "backlog-query":
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"claimed-item": "301"}}, nil
			case "open-pr":
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"pull-request-url": "https://github.com/acme/web/pull/1"}}, nil
			case "ci-poll":
				ciPollCalls++
				if ciPollCalls == 1 {
					auto.mu.Lock()
					auto.ciStatus = "failing"
					auto.mu.Unlock()
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"ciStatus": "failing"}}, nil
				}
				auto.mu.Lock()
				auto.ciStatus = "passing"
				auto.mu.Unlock()
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"ciStatus": "passing"}}, nil
			case "issue-close-out":
				closeOutCalls++
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			default: // "ci" (make ci / local-ci)
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
		},
	}

	inv := &fakeInvoker{
		invoke: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			mu.Lock()
			implementCalls++
			mu.Unlock()
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"changed-files": []interface{}{"main.go"}}}, nil
		},
		review: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det, Auto: auto})

	env.ExecuteWorkflow(Run, implementationRunInput(spec))

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}

	// implement ran twice: once before the CI failure, once on repass.
	if implementCalls != 2 {
		t.Errorf("implement calls = %d, want 2 (initial + one CI-fail repass)", implementCalls)
	}
	if ciPollCalls != 2 {
		t.Errorf("ci-poll calls = %d, want 2 (fail then pass)", ciPollCalls)
	}
	if closeOutCalls != 1 {
		t.Errorf("close-out calls = %d, want 1", closeOutCalls)
	}
	if _, ok := res.Outputs["close-out"]; !ok {
		t.Error("expected close-out to have run and recorded an output")
	}
}

// TestImplementationDryRunReviewerRepassThenApprove exercises issue #27's
// reviewer-path acceptance scenario: reviewer gate ON, requests changes once,
// approves on repass, then completes. CI/local-ci always pass so the
// reviewer loop is isolated.
func TestImplementationDryRunReviewerRepassThenApprove(t *testing.T) {
	spec := loadImplementationWorkflow(t)

	auto := &fixtureAuto{localOK: true, ciStatus: "passing"}
	var mu sync.Mutex
	implementCalls, reviewCalls := 0, 0
	var reviewDecisions []apiv1.VerdictDecision

	det := &fakeRunner{
		run: func(_ context.Context, _ apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			if len(r.Command) == 0 {
				return apiv1.ResultEnvelope{}, nil
			}
			switch r.Command[1] {
			case "backlog-query":
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"claimed-item": "302"}}, nil
			case "open-pr":
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"pull-request-url": "https://github.com/acme/web/pull/2"}}, nil
			case "ci-poll":
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"ciStatus": "passing"}}, nil
			case "issue-close-out":
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			default:
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
		},
	}

	inv := &fakeInvoker{
		invoke: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			mu.Lock()
			implementCalls++
			mu.Unlock()
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"changed-files": []interface{}{"main.go"}}}, nil
		},
		review: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			mu.Lock()
			reviewCalls++
			n := reviewCalls
			mu.Unlock()
			if n == 1 {
				v := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "missing test coverage for the new branch"}
				reviewDecisions = append(reviewDecisions, v.Decision)
				return v, nil
			}
			v := apiv1.Verdict{Decision: apiv1.VerdictPass}
			reviewDecisions = append(reviewDecisions, v.Decision)
			return v, nil
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det, Auto: auto})

	env.ExecuteWorkflow(Run, implementationRunInput(spec))

	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}

	if reviewCalls != 2 {
		t.Fatalf("review calls = %d, want 2 (needs-changes then pass)", reviewCalls)
	}
	if len(reviewDecisions) != 2 || reviewDecisions[0] != apiv1.VerdictNeedsChanges || reviewDecisions[1] != apiv1.VerdictPass {
		t.Errorf("review decisions = %v, want [needs-changes pass]", reviewDecisions)
	}
	// implement ran twice: once before review's needs-changes, once on repass.
	if implementCalls != 2 {
		t.Errorf("implement calls = %d, want 2 (initial + one reviewer repass)", implementCalls)
	}
}
