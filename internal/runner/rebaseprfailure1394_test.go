package runner

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestShippedPRRemediationFailedRebaseReachesCheckpoint(t *testing.T) {
	const runID = "prr-rebase-failure"
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newRebindFixtureRepo(t)

	var mu sync.Mutex
	visited := []string{}
	byTask := map[string]stubTaskResult{
		runID + ":update-behind-pr": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			"selectedNumber": "77", "needsFullRemediation": "true",
		}},
		runID + ":gather-pr-context": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			WorkspaceBranchOutput:    rebindBranch,
			"selectedNumber":         "77",
			"head":                   rebindBranch,
			"base":                   "main",
			"isBehindBase":           "true",
			"hasSubstantiveFindings": "true",
			"hasFailingCI":           "false",
		}},
		runID + ":gather-ci-failures": {
			status: apiv1.ResultSuccess,
			outputs: map[string]interface{}{
				WorkspaceBranchOutput:    rebindBranch,
				"selectedNumber":         "77",
				"head":                   rebindBranch,
				"base":                   "main",
				"isBehindBase":           "true",
				"hasSubstantiveFindings": "true",
				"hasFailingCI":           "false",
			},
			artifactName:      "remediation-brief.json",
			artifactData:      []byte(`{"schema":"goobers.dev/remediation-brief/v1","selectedNumber":"77"}`),
			artifactMediaType: "application/json",
		},
		runID + ":rebase-pr": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{
			Code: "provider_error", Message: "rebase failed",
		}, outputs: map[string]interface{}{
			"selectedNumber": "77", "head": rebindBranch, "needsAgent": "true",
			"conflict": "false", "conflictLocations": "[]", "attemptedHeadSha": "", "rebaseBaseSha": "",
		}},
		runID + ":remediation-checkpoint": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			"continueRemediation": "false", "selectedNumber": "77",
			"head": rebindBranch, "headSha": "deadbeef",
		}},
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &visitRecordingDeterministic{t: t, rec: rec, byTask: byTask, mu: &mu, visited: &visited}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return &remediationGoober{t: t}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		ScratchDir:   filepath.Join(instanceRoot, "scratch"),
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: loadShippedPRRemediation(t), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q (visited: %v)", res.Phase, journal.PhaseCompleted, visited)
	}
	want := []string{"update-behind-pr", "gather-pr-context", "gather-ci-failures", "rebase-pr", "remediation-checkpoint"}
	if strings.Join(visited, ",") != strings.Join(want, ",") {
		t.Fatalf("stage order = %v, want %v", visited, want)
	}
}
