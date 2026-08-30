package runner

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/baseline"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// stubBaselineHealth is the #2971 seam under test: it records what the runner
// asked about and answers with a fixed classification.
type stubBaselineHealth struct {
	baseSHA  string
	decision baseline.Decision
	requests []baseline.Request
}

func (s *stubBaselineHealth) BaseSHA(context.Context, apiv1.RepoRef, string) (string, error) {
	return s.baseSHA, nil
}

func (s *stubBaselineHealth) Classify(_ context.Context, req baseline.Request) (baseline.Decision, error) {
	s.requests = append(s.requests, req)
	return s.decision, nil
}

const baselineCIFailureSummary = "command exited 2"

func runLocalCIFailure(t *testing.T, runID string, health BaselineHealth) (Result, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":" + localCIStageName: {
					status:  apiv1.ResultFailure,
					summary: baselineCIFailureSummary,
					errorInfo: &apiv1.ErrorInfo{
						Code:      "nonzero_exit",
						Message:   "command exited 2; stderr: agent-instructions-validation.test.ts:42 expected 3 sections",
						Retryable: false,
					},
				},
			}}, nil
		},
		BaselineHealth: health,
		Worktrees:      wtMgr,
		RunsDir:        runsDir,
		RepoCloneURL:   func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: localCIFixtureMachine(t, []string{"make", "ci"}),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "101", Provider: apiv1.ProviderGitHub, Title: "unrelated feature"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return res, runsDir
}

func baselineAnnotations(t *testing.T, runsDir, runID string) []map[string]any {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var out []map[string]any
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == baselineClassificationKind {
			out = append(out, event.Runner)
		}
	}
	return out
}

// TestSharedBaselineFailureParksTheRun is #2971's regression: a local-ci
// failure the target branch already reproduces at the pinned base SHA must not
// route back into an implementation repass that can only re-derive an empty
// diff. It becomes the recognized non-retryable SHARED_BASELINE_FAILURE
// disposition, so the run parks against the shared blocker on attempt one.
func TestSharedBaselineFailureParksTheRun(t *testing.T) {
	const runID = "run-shared-baseline"
	health := &stubBaselineHealth{
		baseSHA: "abc123def4567890",
		decision: baseline.Decision{
			Class:      baseline.ClassSharedBaselineFailure,
			BaseSHA:    "abc123def4567890",
			BlockerKey: "acme/web@0f1e2d3c4b5a",
			Waiting:    2,
			Park:       true,
			Reason:     "identical failure on the target branch at base abc123def456",
		},
	}

	res, runsDir := runLocalCIFailure(t, runID, health)

	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated: a shared baseline failure parks instead of repassing", res.Phase)
	}
	if len(health.requests) != 1 {
		t.Fatalf("classify calls = %d, want exactly 1", len(health.requests))
	}
	req := health.requests[0]
	if req.Repo != "acme/web" || req.BaseSHA != "abc123def4567890" {
		t.Fatalf("request repo/base = %q/%q, want acme/web at the pinned base SHA", req.Repo, req.BaseSHA)
	}
	if baseline.CommandKey(req.Command) != baseline.CommandKey([]string{"make", "ci"}) {
		t.Fatalf("request command = %v, want the configured local-ci command", req.Command)
	}
	if req.Waiter != "101" || req.RunID != runID {
		t.Fatalf("waiter/run = %q/%q, want the driving item and this run parked on the blocker", req.Waiter, req.RunID)
	}

	annotations := baselineAnnotations(t, runsDir, runID)
	if len(annotations) != 1 {
		t.Fatalf("baseline annotations = %d, want 1 journaled classification", len(annotations))
	}
	got := annotations[0]
	if got["class"] != string(baseline.ClassSharedBaselineFailure) {
		t.Fatalf("annotation class = %v, want %q", got["class"], baseline.ClassSharedBaselineFailure)
	}
	if got["parked"] != true {
		t.Fatalf("annotation parked = %v, want true", got["parked"])
	}
	if got["blocker"] != "acme/web@0f1e2d3c4b5a" {
		t.Fatalf("annotation blocker = %v, want the shared blocker key", got["blocker"])
	}
}

// TestPRIntroducedFailureRoutesUnchanged pins the other half of the
// classification: when the baseline is healthy the failure stays the run's own
// and its pre-existing routing (a plain failed run here) is untouched.
func TestPRIntroducedFailureRoutesUnchanged(t *testing.T) {
	const runID = "run-pr-introduced"
	health := &stubBaselineHealth{
		baseSHA:  "abc123def4567890",
		decision: baseline.Decision{Class: baseline.ClassPRIntroduced, Reason: "baseline abc123def456 is green for this command"},
	}

	res, runsDir := runLocalCIFailure(t, runID, health)

	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed: a branch-introduced CI failure routes exactly as before", res.Phase)
	}
	if res.FailureCode != "nonzero_exit" {
		t.Fatalf("failure code = %q, want the original nonzero_exit", res.FailureCode)
	}
	annotations := baselineAnnotations(t, runsDir, runID)
	if len(annotations) != 1 || annotations[0]["parked"] != false {
		t.Fatalf("annotations = %+v, want one classification recording that nothing was parked", annotations)
	}
}

// TestBaselineHealthUnconfiguredIsInert keeps the seam opt-in: with no
// evaluator wired, no classification is attempted and no annotation appears.
func TestBaselineHealthUnconfiguredIsInert(t *testing.T) {
	const runID = "run-no-baseline-health"

	res, runsDir := runLocalCIFailure(t, runID, nil)

	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if annotations := baselineAnnotations(t, runsDir, runID); len(annotations) != 0 {
		t.Fatalf("annotations = %+v, want none without a configured evaluator", annotations)
	}
}

func TestBaselineCandidateOnlyMatchesGenericLocalCIFailures(t *testing.T) {
	localCI := apiv1.Task{Name: localCIStageName, Type: apiv1.TaskDeterministic}
	failure := func(code string) apiv1.ResultEnvelope {
		return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: code}}
	}

	cases := []struct {
		name   string
		task   apiv1.Task
		result apiv1.ResultEnvelope
		want   bool
	}{
		{"local ci nonzero exit", localCI, failure("nonzero_exit"), true},
		{"typed sync conflict", localCI, failure(BaseSyncConflictErrorCode), false},
		{"another stage", apiv1.Task{Name: "implement", Type: apiv1.TaskDeterministic}, failure("nonzero_exit"), false},
		{"agentic stage named local-ci", apiv1.Task{Name: localCIStageName, Type: apiv1.TaskAgentic}, failure("nonzero_exit"), false},
		{"success", localCI, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, false},
		{"failure without error detail", localCI, apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := baselineCandidate(tc.task, tc.result); got != tc.want {
				t.Fatalf("baselineCandidate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyBaselineDecision(t *testing.T) {
	original := apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: baselineCIFailureSummary,
		Error:   &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 2"},
	}

	parked, changed := applyBaselineDecision(original, baseline.Decision{
		Class: baseline.ClassSharedBaselineFailure, Park: true, Reason: "identical failure at base abc123",
	})
	if !changed {
		t.Fatal("changed = false, want the parked disposition applied")
	}
	if parked.Error.Code != SharedBaselineFailureCode || parked.Error.Retryable {
		t.Fatalf("error = %+v, want a non-retryable %s", parked.Error, SharedBaselineFailureCode)
	}
	if !isNonRetryableEscalation(parked.Error) {
		t.Fatal("the parked disposition must be a recognized non-retryable escalation")
	}

	for _, decision := range []baseline.Decision{
		{Class: baseline.ClassPRIntroduced},
		{Class: baseline.ClassUnknown},
		{Class: baseline.ClassSharedBaselineFailure, Park: false},
	} {
		got, changed := applyBaselineDecision(original, decision)
		if changed || got.Error.Code != "nonzero_exit" {
			t.Fatalf("decision %+v rewrote the result to %+v, want it untouched", decision, got.Error)
		}
	}
}

func TestBaselineFailureTextCarriesTheStageDiagnostic(t *testing.T) {
	got := baselineFailureText(apiv1.ResultEnvelope{
		Summary: "command exited 2",
		Error:   &apiv1.ErrorInfo{Message: "agent-instructions-validation.test.ts:42"},
	})
	if got != "command exited 2\nagent-instructions-validation.test.ts:42" {
		t.Fatalf("failure text = %q, want summary and error message joined", got)
	}
	if got := baselineFailureText(apiv1.ResultEnvelope{}); got != "" {
		t.Fatalf("failure text = %q, want empty for a result with no evidence", got)
	}
}

func TestCICommandReturnsTheConfiguredArgv(t *testing.T) {
	machine := localCIFixtureMachine(t, []string{"make", "ci", "test-integration-strict"})
	got := ciCommand(machine)
	if baseline.CommandKey(got) != baseline.CommandKey([]string{"make", "ci", "test-integration-strict"}) {
		t.Fatalf("ciCommand = %v, want the full configured argv", got)
	}
	if ciCommand(agenticGateMachine(t)) != nil {
		t.Fatal("ciCommand = non-nil for a machine with no local-ci stage")
	}
}
