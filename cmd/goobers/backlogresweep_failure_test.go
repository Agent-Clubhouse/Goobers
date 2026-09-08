package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestBacklogResweepDependencyFailureDoesNotAdvanceSweep(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Parked item", providers.LabelApproved, blockedOnSiblingLabel)
	server.dependencyFailureStatus = map[int]int{7: http.StatusForbidden}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "resweep-failure")
	configureCurationResweep(t, "2", "2")
	t.Setenv("GOOBERS_INPUT_RECONCILEMETADATA", "false")
	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "backlog-query", "--claim", "--resweep", root)
	if code == 0 || !strings.Contains(stderr, "dependency recheck item 7") {
		t.Fatalf("dependency failure hidden: code=%d stderr=%q", code, stderr)
	}
	raw, err := os.ReadFile("claimed-items.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload[executor.OutputErrorCode] == nil || payload[executor.OutputErrorRetryable] != false {
		t.Fatalf("missing typed nonretryable permission failure: %+v", payload)
	}
	states, err := filepath.Glob(filepath.Join(root, "scheduler", "backlog-resweep-*.json"))
	if err != nil || len(states) != 0 {
		t.Fatalf("failed sweep advanced its cursor/cooldown: states=%v err=%v", states, err)
	}
}
