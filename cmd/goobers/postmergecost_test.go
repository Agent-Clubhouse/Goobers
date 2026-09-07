package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

type staticCostCommentReader map[string][]providers.Comment

func (r staticCostCommentReader) ListComments(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.Comment, error) {
	return r[id], nil
}

func TestCollectPostMergeCostReportDeduplicatesRunsAndAllocatesIssues(t *testing.T) {
	implementationOld := int64(4_000_000_000)
	implementationLatest := int64(8_000_000_000)
	review := int64(2_000_000_000)
	prReader := staticCostCommentReader{
		"77": {
			costComment(t, "goobers", "implementation", "run-impl", 10, implementationOld),
			costComment(t, "goobers", "implementation", "run-impl", 20, implementationLatest),
			costComment(t, "goobers", "merge-review", "run-review", 30, review),
		},
	}
	issueReader := staticCostCommentReader{
		"10": {costComment(t, "goobers", "implementation", "run-impl", 10, implementationOld)},
		"11": nil,
	}

	report, err := collectPostMergeCostReport(
		context.Background(),
		prReader,
		issueReader,
		providers.RepositoryRef{},
		providers.RepositoryRef{},
		"77",
		[]string{"10", "11"},
		"goobers",
		"goobers",
	)
	if err != nil {
		t.Fatalf("collectPostMergeCostReport: %v", err)
	}
	if got, want := len(report.Receipts), 2; got != want {
		t.Fatalf("receipt count = %d, want %d", got, want)
	}
	if got, want := *report.Total.NanoAIU, int64(10_000_000_000); got != want {
		t.Fatalf("total nano-AIU = %d, want %d", got, want)
	}
	// The implementation run is directly associated with issue 10. The
	// PR-only review run follows that direct-cost weight, so all cost remains
	// attributable to issue 10 rather than being counted twice.
	if got, want := report.IssueNanoAIU["10"], int64(10_000_000_000); got != want {
		t.Fatalf("issue 10 nano-AIU = %d, want %d", got, want)
	}
	if got := report.IssueNanoAIU["11"]; got != 0 {
		t.Fatalf("issue 11 nano-AIU = %d, want 0", got)
	}
	if got, want := *report.ByWorkflow["implementation"].NanoAIU, implementationLatest; got != want {
		t.Fatalf("implementation nano-AIU = %d, want %d", got, want)
	}
}

func TestCollectPostMergeCostReportIgnoresUntrustedComments(t *testing.T) {
	value := int64(9_000_000_000)
	reader := staticCostCommentReader{
		"77": {costComment(t, "someone-else", "implementation", "run-impl", 10, value)},
	}
	report, err := collectPostMergeCostReport(
		context.Background(),
		reader,
		reader,
		providers.RepositoryRef{},
		providers.RepositoryRef{},
		"77",
		nil,
		"goobers",
		"goobers",
	)
	if err != nil {
		t.Fatalf("collectPostMergeCostReport: %v", err)
	}
	if report.Total.NanoAIU != nil || len(report.Receipts) != 0 {
		t.Fatalf("untrusted receipt was counted: %+v", report)
	}
}

func TestRenderPostMergeCostComments(t *testing.T) {
	total := int64(12_400_000_000)
	implementation := int64(9_000_000_000)
	review := int64(3_400_000_000)
	report := postMergeCostReport{
		Total:        providers.CostReceipt{NanoAIU: &total},
		ByWorkflow:   map[string]providers.CostReceipt{"merge-review": {NanoAIU: &review}, "implementation": {NanoAIU: &implementation}},
		IssueNanoAIU: map[string]int64{"42": 7_000_000_000},
	}

	closeOut := mergedPullRequestComment("77", report, "42")
	for _, want := range []string{
		"Merged in pull request #77.",
		"**Total Goobers cost for this PR:** 12.40 AIC",
		"**Cost attributed to this issue:** 7.00 AIC",
	} {
		if !strings.Contains(closeOut, want) {
			t.Fatalf("close-out comment %q does not contain %q", closeOut, want)
		}
	}

	summary := renderPostMergeCostSummary(report)
	for _, want := range []string{
		"Thanks for using Goobers. Your cost for this PR was **12.40 AIC**.",
		"- `implementation`: 9.00 AIC",
		"- `merge-review`: 3.40 AIC",
		postMergeCostSummaryMarker,
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q does not contain %q", summary, want)
		}
	}
}

func TestPostMergePublishesSummaryAndIssueAllocationFromReceipts(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	st.prComments = []string{costComment(t, "goobers", "merge-review", "run-review", 30, 2_000_000_000).Body}
	st.issueComments[42] = []string{costComment(t, "goobers", "implementation", "run-impl", 20, 8_000_000_000).Body}
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root, _ := postMergeEnv(t, server.URL, false, map[string]string{"pullNumber": "20"})

	code, _, stderr := runArgs(t, "post-merge", root)
	if code != 0 {
		t.Fatalf("post-merge code = %d, stderr = %q", code, stderr)
	}

	st.mu.Lock()
	prComments := append([]string(nil), st.prComments...)
	issueComments := append([]string(nil), st.issueComments[42]...)
	st.mu.Unlock()
	if len(prComments) != 2 || !strings.Contains(prComments[1], "Your cost for this PR was **10.00 AIC**") {
		t.Fatalf("pull request comments = %q, want one 10.00 AIC summary", prComments)
	}
	if len(issueComments) != 2 ||
		!strings.Contains(issueComments[1], "**Total Goobers cost for this PR:** 10.00 AIC") ||
		!strings.Contains(issueComments[1], "**Cost attributed to this issue:** 10.00 AIC") {
		t.Fatalf("issue comments = %q, want total and issue allocation", issueComments)
	}
}

func costComment(t *testing.T, author, workflow, runID string, sequence uint64, nanoAIU int64) providers.Comment {
	t.Helper()
	attribution := providers.Attribution{
		Schema:   1,
		Goobers:  true,
		Gaggle:   "test",
		Workflow: workflow,
		Task:     "task",
		Goober:   "goober",
		Run:      runID,
		Action:   "comment",
		Cost:     &providers.CostReceipt{JournalSequence: sequence, NanoAIU: &nanoAIU},
	}
	data, err := json.Marshal(attribution)
	if err != nil {
		t.Fatalf("marshal attribution: %v", err)
	}
	return providers.Comment{
		Author: author,
		Body:   providers.AttributionMarkerPrefix + base64.StdEncoding.EncodeToString(data) + " -->",
	}
}
