package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #6134: on GitHub rebase-pr's human-comment cause reads the same open
// feedback merge-pr refuses to land over, so an unanswered inline review
// thread the gate routes here is acted on rather than cleared; a resolved
// thread is not feedback.
func TestRebasePRReadsUnresolvedReviewThreads(t *testing.T) {
	for i, resolved := range []bool{false, true} {
		name := "unresolved"
		if resolved {
			name = "resolved"
		}
		t.Run(name, func(t *testing.T) {
			prBranch := "goobers/impl/run-6134-" + name
			origin := initNonConflictingPRBranch(t, prBranch)
			wt := prWorktree(t, origin, prBranch)
			st := &rebasePRServerState{
				labels: []string{needsRemediationLabel},
				reviewComments: []map[string]interface{}{{
					"id": 901, "body": "Please add a README note.", "path": "a.go",
					"created_at": "2026-10-01T12:00:00Z", "user": map[string]string{"login": "alice"},
				}},
				reviewThreadResolved: resolved,
			}
			number := 61 + i
			server := st.start(t, "your-org", "your-repo", number)
			instanceRoot := rebasePREnv(t, server.URL, wt.Path, map[string]string{
				"selectedNumber": []string{"61", "62"}[i], "head": prBranch, "base": "main",
				"hasSubstantiveFindings": "false", "hasFailingCI": "false", "remediate": defaultRemediatePolicy,
			})
			if code, stdout, stderr := runArgs(t, "rebase-pr", instanceRoot); code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
			if err != nil {
				t.Fatal(err)
			}
			fired := strings.Contains(string(data), `"remediationCauses":"human-comment"`)
			if fired == resolved {
				t.Fatalf("rebase-result.json = %s, want human-comment fired=%v", data, !resolved)
			}
		})
	}
}
