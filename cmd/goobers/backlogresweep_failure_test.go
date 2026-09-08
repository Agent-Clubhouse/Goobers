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
	for _, tc := range []struct {
		name      string
		status    int
		retryable bool
	}{
		{"permission", http.StatusForbidden, false},
		{"unavailable", http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Parked item", providers.LabelApproved, blockedOnSiblingLabel)
			server.dependencyFailureStatus = map[int]int{7: tc.status}
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
			if payload[executor.OutputErrorCode] == nil || payload[executor.OutputErrorRetryable] != tc.retryable {
				t.Fatalf("missing correctly classified provider failure: %+v", payload)
			}
			states, err := filepath.Glob(filepath.Join(root, "scheduler", "backlog-resweep-*.json"))
			if err != nil || len(states) != 0 {
				t.Fatalf("failed sweep advanced its cursor/history: states=%v err=%v", states, err)
			}
		})
	}
}
