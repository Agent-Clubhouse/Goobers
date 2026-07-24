package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func seedReviewThreadsBrief(t *testing.T, root, runID string, brief apiv1.RemediationBrief) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create remediation run journal: %v", err)
	}
	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatalf("marshal remediation brief: %v", err)
	}
	if _, err := run.RecordArtifact(runID+":gather-pr-context/result", data); err != nil {
		t.Fatalf("record remediation brief: %v", err)
	}
	if _, err := run.RecordArtifact(runID+":gather-sibling-context/result", []byte(`{"siblings":[]}`)); err != nil {
		t.Fatalf("record sibling context: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close remediation run journal: %v", err)
	}
}

func reviewThreadsBrief() apiv1.RemediationBrief {
	return apiv1.RemediationBrief{
		Schema:                 apiv1.RemediationBriefVersion,
		SelectedNumber:         "77",
		Head:                   "goobers/implementation/run-77",
		Base:                   "main",
		WorkspaceBranch:        "goobers/implementation/run-77",
		IsBehindBase:           true,
		HasSubstantiveFindings: "true",
		HasFailingCI:           "false",
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA: "head-sha",
			BaseSHA: "base-sha",
			Comments: []apiv1.RemediationThreadComment{
				{Author: "reviewer", Body: "Keep this issue-level context."},
			},
		},
		GatherCIFailures: &apiv1.RemediationCIFailures{
			Checks: []apiv1.RemediationCIFailure{},
		},
	}
}

func TestGatherReviewThreadsAddsReviewEvidenceAndPreservesBrief(t *testing.T) {
	const runID = "run-939"
	root := initDemo(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/your-org/your-repo/pulls/77/reviews":
			_, _ = w.Write([]byte(`[{"id":1,"user":{"login":"goobers-bot"},"state":"CHANGES_REQUESTED","body":"Fix this.","commit_id":"head-sha","submitted_at":"2026-07-23T10:00:00Z","html_url":"https://example/reviews/1"}]`))
		case "/repos/your-org/your-repo/pulls/77/comments":
			_, _ = w.Write([]byte(`[{"id":101,"user":{"login":"reviewer"},"body":"Guard this write.","path":"worker.go","line":42,"original_line":40,"side":"RIGHT","start_line":40,"original_start_line":38,"start_side":"RIGHT","diff_hunk":"@@ -38,3 +40,5 @@","created_at":"2026-07-23T10:05:00Z","html_url":"https://example/comments/101"}]`))
		case "/graphql":
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false,"isOutdated":false,"path":"worker.go","line":42,"originalLine":40,"diffSide":"RIGHT","startLine":40,"originalStartLine":38,"startDiffSide":"RIGHT","comments":{"nodes":[{"databaseId":101}]}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	original := reviewThreadsBrief()
	seedReviewThreadsBrief(t, root, runID, original)
	previousProvider := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		provider := providers.NewGitHubProvider(token, opts...)
		provider.BaseURL = server.URL
		return provider
	}
	t.Cleanup(func() { newGitHubProvider = previousProvider })
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "gather-review-threads", root); code != 0 {
		t.Fatalf("gather-review-threads: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal remediation brief: %v", err)
	}

	want := original
	want.GatherReviewThreads = &apiv1.RemediationReviewThreads{
		Reviews: []apiv1.RemediationNativeReview{{
			Author: "goobers-bot", State: "CHANGES_REQUESTED", Body: "Fix this.",
			CommitSHA: "head-sha", SubmittedAt: "2026-07-23T10:00:00Z", URL: "https://example/reviews/1",
		}},
		InlineComments: []apiv1.RemediationInlineComment{{
			Author: "reviewer", Body: "Guard this write.", Path: "worker.go",
			Line: 42, OriginalLine: 40, Side: "RIGHT", DiffHunk: "@@ -38,3 +40,5 @@",
			StartLine: 40, OriginalStartLine: 38, StartSide: "RIGHT",
			CreatedAt: "2026-07-23T10:05:00Z", URL: "https://example/comments/101",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remediation brief = %#v, want %#v", got, want)
	}
}
