package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestDecompositionStageArtifactSelectsLatestSuccessfulStageResult(t *testing.T) {
	oldRef := journal.Ref{Path: "artifacts/old", Digest: "sha256:old"}
	newRef := journal.Ref{Path: "artifacts/new", Digest: "sha256:new"}
	events := []journal.Event{
		{Type: journal.EventArtifactRecorded, Name: "run:select-source/result", Ref: &oldRef},
		{Type: journal.EventStageFinished, Stage: "select-source", Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{oldRef}},
		{Type: journal.EventArtifactRecorded, Name: "run:select-source/result", Ref: &newRef},
		{Type: journal.EventStageFinished, Stage: "select-source", Status: string(apiv1.ResultFailure), Artifacts: []journal.Ref{newRef}},
	}

	got, ok := decompositionStageArtifact(events, "select-source", "/result")
	if !ok || got.Digest != oldRef.Digest {
		t.Fatalf("artifact = %+v, %v; want latest successful ref %+v", got, ok, oldRef)
	}
}

func (s *fakeGitHubServer) setIssueTitle(number int, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number].title = title
}

func validDecompositionPlan(selection decomposition.Selection) decomposition.Plan {
	return decomposition.Plan{
		SchemaVersion: decomposition.PlanSchemaV1,
		Selection: decomposition.PlanSelection{
			Mode:                selection.Mode,
			SourceRunID:         selection.SourceRunID,
			IssueSnapshotDigest: selection.IssueSnapshotDigest,
		},
		Parent:  selection.Parent,
		Summary: "Split the large issue into a selector and a validator.",
		Children: []decomposition.ChildPlan{
			{
				Key:                "selector",
				Title:              "Add decomposition disposition selection",
				Body:               "Implement select-source to find and claim an unconsumed L6 disposition.",
				AcceptanceCriteria: "Fixtures cover every excluded escalation class.",
				ValidationBoundary: "unit tests over the selector logic",
				Labels:             []string{"area:workflows", "type:feature"},
			},
			{
				Key:                "validator",
				Title:              "Add decomposition-plan schema and validator",
				Body:               "Implement validate-plan to check the plan produced by design-slices.",
				AcceptanceCriteria: "Invalid plans produce zero provider mutations.",
				ValidationBoundary: "unit tests over the validator logic",
				Labels:             []string{"area:workflows", "type:feature"},
				DependsOn:          []string{"selector"},
			},
		},
	}
}

// TestValidatePlanAgainstRealSelectSourceOutput drives select-source first —
// the actual CLI entrypoint DEC-1 ships, not a hand-built stand-in — and feeds
// its real selection.json into validate-plan, matching the acceptance
// boundary's "tested against DEC-1's actual output" requirement.
func TestValidatePlanAgainstRealSelectSourceOutput(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-1",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "419",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large to implement as one PR"),
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(419, "A very large issue", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "test-token")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)

	if code, stdout, stderr := runArgs(t, "select-source", root); code != 0 {
		t.Fatalf("select-source: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	selectionData, err := os.ReadFile(filepath.Join(workDir, "selection.json"))
	if err != nil {
		t.Fatalf("read selection.json: %v", err)
	}
	var selection decomposition.Selection
	if err := json.Unmarshal(selectionData, &selection); err != nil {
		t.Fatalf("unmarshal selection.json: %v", err)
	}

	plan := validDecompositionPlan(selection)
	planData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "plan.json"), planData, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate-plan", root)
	if code != 0 {
		t.Fatalf("validate-plan: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "plan-validation.json"))
	if err != nil {
		t.Fatalf("read plan-validation.json: %v", err)
	}
	var got validatePlanResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal plan-validation.json: %v", err)
	}
	if !got.Valid {
		t.Fatalf("plan-validation = %+v, stdout = %q, want valid", got, stdout)
	}
	if got.PlanDigest == "" {
		t.Fatalf("plan-validation = %+v, want digest binding for publisher", got)
	}
}

func TestValidatePlanDetectsLiveParentConflict(t *testing.T) {
	if runtime.GOOS == "windows" {
		// This test hangs for the entire 30-minute package timeout on
		// Windows CI, blocked inside apiReadCache's lock.Acquire during the
		// first select-source invocation's GetWorkItem call. Static reading
		// ruled out a same-file self-deadlock in withBlockingFileLock/
		// apiReadCache (see the linked issue), but nothing short of a
		// Windows box with retry-loop instrumentation can confirm the real
		// cause. Skip pending that investigation rather than eating the
		// whole windows-smoke budget on every PR. Tracking: #2590.
		t.Skip("hangs ~30m on Windows CI, see #2590")
	}
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-2",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "420",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large to implement as one PR"),
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(420, "A very large issue", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "test-token")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, _, stderr := runArgs(t, "select-source", root); code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	selectionData, err := os.ReadFile(filepath.Join(workDir, "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var selection decomposition.Selection
	if err := json.Unmarshal(selectionData, &selection); err != nil {
		t.Fatal(err)
	}
	plan := validDecompositionPlan(selection)
	planData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "plan.json"), planData, 0o644); err != nil {
		t.Fatal(err)
	}

	// The parent changes after selection but before design-slices' plan is
	// validated — a maintainer retitled it while decomposition was in flight.
	server.setIssueTitle(420, "A retitled issue since selection")

	code, stdout, stderr := runArgs(t, "validate-plan", root)
	if code != 0 {
		t.Fatalf("validate-plan: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "plan-validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got validatePlanResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Valid || !got.Conflict {
		t.Fatalf("plan-validation = %+v, want a conflict, not an ordinary valid/invalid result", got)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("plan-validation errors = %v, want none alongside a conflict", got.Errors)
	}
}

func TestValidatePlanEmitsScalarUnresolvedDecisionSignal(t *testing.T) {
	root := t.TempDir()
	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(422, "A product decision is needed", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "test-token")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	selection := decomposition.Selection{
		Mode: decomposition.SelectionModeEscalation, SourceRunID: "r1",
		Parent: decomposition.ParentRef{Provider: "github", Repository: "acme/widgets", ID: "422"},
	}
	digest, err := decomposition.IssueSnapshotDigest(
		"422", "A product decision is needed", "", []string{providers.LabelApproved}, "open",
	)
	if err != nil {
		t.Fatal(err)
	}
	selection.IssueSnapshotDigest = digest
	plan := validDecompositionPlan(selection)
	plan.UnresolvedDecision = "Should this preserve the legacy API?"
	selectionData, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("selection.json", selectionData, 0o644); err != nil {
		t.Fatal(err)
	}
	planData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("plan.json", planData, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate-plan", root)
	if code != 0 {
		t.Fatalf("validate-plan: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	resultData, err := os.ReadFile("plan-validation.json")
	if err != nil {
		t.Fatal(err)
	}
	var got validatePlanResult
	if err := json.Unmarshal(resultData, &got); err != nil {
		t.Fatal(err)
	}
	if got.Valid || got.Conflict || !got.UnresolvedDecision ||
		got.UnresolvedDecisionReason != plan.UnresolvedDecision {
		t.Fatalf("plan-validation = %+v, want unresolved-decision routing output", got)
	}
}

func TestValidatePlanRejectsUnsupportedSchemaVersionCLI(t *testing.T) {
	root := t.TempDir()
	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(421, "Some issue", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "test-token")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)

	selection := decomposition.Selection{
		Mode: decomposition.SelectionModeEscalation, SourceRunID: "r1",
		Parent: decomposition.ParentRef{Provider: "github", Repository: "acme/widgets", ID: "421"},
	}
	selectionData, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "selection.json"), selectionData, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := validDecompositionPlan(selection)
	plan.SchemaVersion = "v99"
	planData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "plan.json"), planData, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate-plan", root)
	if code != 0 {
		t.Fatalf("validate-plan: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "plan-validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got validatePlanResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("plan-validation = %+v, want invalid for an unsupported schema version", got)
	}
	if !got.SchemaInvalid {
		t.Fatalf("plan-validation = %+v, want distinct schema-invalid outcome", got)
	}
	if got.Repassable {
		t.Fatalf("plan-validation = %+v, schema-invalid outcome must not be repassable", got)
	}
}
