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
		"**Total Goobers cost for this PR:** 12 AIC",
		"**Cost attributed to this issue:** 7 AIC",
	} {
		if !strings.Contains(closeOut, want) {
			t.Fatalf("close-out comment %q does not contain %q", closeOut, want)
		}
	}

	summary := renderPostMergeCostSummary(report)
	for _, want := range []string{
		"Thanks for using Goobers. Your cost for this PR was **12 AIC**.",
		"- `implementation`: 9 AIC",
		"- `merge-review`: 3 AIC",
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
	if len(prComments) != 2 || !strings.Contains(prComments[1], "Your cost for this PR was **10 AIC**") {
		t.Fatalf("pull request comments = %q, want one 10 AIC summary", prComments)
	}
	if len(issueComments) != 2 ||
		!strings.Contains(issueComments[1], "**Total Goobers cost for this PR:** 10 AIC") ||
		!strings.Contains(issueComments[1], "**Cost attributed to this issue:** 10 AIC") {
		t.Fatalf("issue comments = %q, want total and issue allocation", issueComments)
	}
}

// TestPostMergeCostCommentsDisclosePartialCoverage is #6353's coverage half:
// a run that did agent work but published no measured cost (an empty receipt)
// makes the total a lower bound, and both the PR summary and the issue
// close-out say so. A deterministic run's marker carries no receipt, costs
// nothing, and must not count against coverage.
func TestPostMergeCostCommentsDisclosePartialCoverage(t *testing.T) {
	tokens := int64(1200)
	prReader := staticCostCommentReader{"77": {
		costComment(t, "goobers", "implementation", "run-impl", 20, 8_000_000_000),
		{Author: "goobers", Body: attributionBodyForTest(t, "merge-review", "run-review", &providers.CostReceipt{JournalSequence: 5, InputTokens: &tokens})},
		{Author: "goobers", Body: attributionBodyForTest(t, "post-merge", "run-deterministic", nil)},
	}}
	report, err := collectPostMergeCostReport(
		context.Background(), prReader, staticCostCommentReader{},
		providers.RepositoryRef{}, providers.RepositoryRef{}, "77", []string{"42"}, "goobers", "goobers",
	)
	if err != nil {
		t.Fatalf("collectPostMergeCostReport: %v", err)
	}
	if known, total := postMergeCostCoverage(report); known != 1 || total != 2 {
		t.Fatalf("coverage = %d of %d, want 1 of 2", known, total)
	}
	const want = "Cost known for 1 of 2 runs, so this total is a lower bound."
	for name, body := range map[string]string{
		"summary":   renderPostMergeCostSummary(report),
		"close-out": mergedPullRequestComment("77", report, "42"),
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s %q does not disclose partial coverage %q", name, body, want)
		}
		if strings.Contains(body, "estimate") {
			t.Fatalf("%s %q labels an AI-credit-only total as an estimate", name, body)
		}
	}
}

// TestPostMergeCostCommentsOmitCoverageForDeterministicRuns: runs without
// agent work (no receipt) never turn a complete total into a lower bound.
func TestPostMergeCostCommentsOmitCoverageForDeterministicRuns(t *testing.T) {
	prReader := staticCostCommentReader{"77": {
		costComment(t, "goobers", "implementation", "run-impl", 20, 8_000_000_000),
		{Author: "goobers", Body: attributionBodyForTest(t, "post-merge", "run-deterministic", nil)},
	}}
	report, err := collectPostMergeCostReport(
		context.Background(), prReader, staticCostCommentReader{},
		providers.RepositoryRef{}, providers.RepositoryRef{}, "77", []string{"42"}, "goobers", "goobers",
	)
	if err != nil {
		t.Fatalf("collectPostMergeCostReport: %v", err)
	}
	for name, body := range map[string]string{
		"summary":   renderPostMergeCostSummary(report),
		"close-out": mergedPullRequestComment("77", report, "42"),
	} {
		if strings.Contains(body, "lower bound") {
			t.Fatalf("%s %q discloses partial coverage for a deterministic run", name, body)
		}
	}
}

// TestPostMergeCostCommentsLabelVendorEstimates is #6353's estimate half: a
// Claude-sourced receipt is footnoted as a vendor-reported estimate wherever
// its amount appears, and full coverage adds no lower-bound disclosure.
func TestPostMergeCostCommentsLabelVendorEstimates(t *testing.T) {
	billed := int64(8_000_000_000)
	estimated := int64(2_000_000_000)
	prReader := staticCostCommentReader{"77": {
		{Author: "goobers", Body: attributionBodyForTest(t, "implementation", "run-impl", &providers.CostReceipt{JournalSequence: 1, NanoAIU: &billed})},
		{Author: "goobers", Body: attributionBodyForTest(t, "merge-review", "run-review", &providers.CostReceipt{JournalSequence: 1, NanoAIU: &estimated, VendorEstimated: true})},
	}}
	report, err := collectPostMergeCostReport(
		context.Background(), prReader, staticCostCommentReader{},
		providers.RepositoryRef{}, providers.RepositoryRef{}, "77", []string{"42"}, "goobers", "goobers",
	)
	if err != nil {
		t.Fatalf("collectPostMergeCostReport: %v", err)
	}
	const footnote = `\* Includes Claude costs, which are vendor-reported estimates normalized to AIC for totals.`
	summary := renderPostMergeCostSummary(report)
	for _, want := range []string{
		`Your cost for this PR was **10 AIC\***.`,
		"- `implementation`: 8 AIC\n",
		"- `merge-review`: 2 AIC\\*",
		footnote,
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q does not contain %q", summary, want)
		}
	}
	closeOut := mergedPullRequestComment("77", report, "42")
	for _, want := range []string{`**Total Goobers cost for this PR:** 10 AIC\*`, footnote} {
		if !strings.Contains(closeOut, want) {
			t.Fatalf("close-out %q does not contain %q", closeOut, want)
		}
	}
	for name, body := range map[string]string{"summary": summary, "close-out": closeOut} {
		if strings.Contains(body, "lower bound") {
			t.Fatalf("%s %q discloses partial coverage for a complete total", name, body)
		}
	}
}

func attributionBodyForTest(t *testing.T, workflow, runID string, cost *providers.CostReceipt) string {
	t.Helper()
	data, err := json.Marshal(providers.Attribution{
		Schema: 1, Goobers: true, Gaggle: "test", Workflow: workflow,
		Task: "task", Goober: "goober", Run: runID, Action: "comment", Cost: cost,
	})
	if err != nil {
		t.Fatalf("marshal attribution: %v", err)
	}
	return providers.AttributionMarkerPrefix + base64.StdEncoding.EncodeToString(data) + " -->"
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
