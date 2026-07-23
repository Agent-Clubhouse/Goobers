package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
)

func TestSetMilestoneAssignsAndChangesMilestone(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Plan the roadmap")
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubMilestonesWrite)), "run-1")
	t.Setenv(executor.InputEnvVar("itemID"), "7")
	t.Setenv(executor.InputEnvVar("milestone"), "22")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "set-milestone", root)
	if code != 0 {
		t.Fatalf("set-milestone assign: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "issue 7") || !strings.Contains(stdout, "milestone 22") {
		t.Fatalf("stdout = %q, want assigned issue and milestone", stdout)
	}
	server.mu.Lock()
	gotMilestone := server.issues[7].milestone
	server.mu.Unlock()
	if gotMilestone != 22 {
		t.Fatalf("milestone = %d, want 22", gotMilestone)
	}

	t.Setenv(executor.InputEnvVar("milestone"), "23")
	code, stdout, stderr = runArgs(t, "set-milestone", root)
	if code != 0 {
		t.Fatalf("set-milestone change: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	server.mu.Lock()
	gotMilestone = server.issues[7].milestone
	server.mu.Unlock()
	if gotMilestone != 23 {
		t.Fatalf("changed milestone = %d, want 23", gotMilestone)
	}

	facts := readMutationFacts(t, workDir)
	if len(facts) != 2 {
		t.Fatalf("mutation facts = %#v, want two milestone assignments", facts)
	}
	for _, fact := range facts {
		if fact.Provider != "github" || fact.Kind != "issue" || fact.ID != "7" || fact.Operation != "milestone" {
			t.Fatalf("unexpected mutation fact: %+v", fact)
		}
	}
	result := readProviderStageResult(t, filepath.Join(workDir, "milestone-result.json"))
	if result["itemId"] != "7" || result["milestone"] != float64(23) {
		t.Fatalf("milestone result = %#v, want item 7 milestone 23", result)
	}
}

func TestSetMilestoneRequiresMilestoneCapability(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Plan the roadmap")
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "run-1")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubMilestonesWrite)), "")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "set-milestone", "--item", "7", "--milestone", "22", root)
	if code != 1 {
		t.Fatalf("set-milestone without capability: code=%d stderr=%q, want 1", code, stderr)
	}
	if !strings.Contains(stderr, executor.CredentialEnvVar(string(capability.GitHubMilestonesWrite))) {
		t.Fatalf("stderr = %q, want missing milestone capability credential", stderr)
	}
	server.mu.Lock()
	gotMilestone := server.issues[7].milestone
	server.mu.Unlock()
	if gotMilestone != 0 {
		t.Fatalf("milestone changed without capability: %d", gotMilestone)
	}
	if _, err := os.Stat(filepath.Join(workDir, mutationsSidecarFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutation sidecar exists without capability: %v", err)
	}
}

func TestSetMilestoneUsesDefaultResultFileOnFailure(t *testing.T) {
	unsetProviderResultFile(t)
	t.Setenv(executor.InputEnvVar("itemID"), "")
	t.Setenv(executor.InputEnvVar("milestone"), "")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "set-milestone", "--milestone", "22")
	if code != 1 {
		t.Fatalf("set-milestone without item: code=%d stderr=%q, want 1", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(workDir, "milestone-result.json"))
	if result[executor.OutputErrorCode] != errorCodeProvider {
		t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], errorCodeProvider)
	}
	message, _ := result[executor.OutputErrorMessage].(string)
	if !strings.Contains(message, "item is required") {
		t.Fatalf("errorMessage = %q, want missing item error", message)
	}
}
