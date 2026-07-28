package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
)

func routeMergeReviewTestRepo(t *testing.T) {
	t.Helper()
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
}

func TestPRSelectAuthorScopeAnySelectsOutsidePrefixesAsAdvisory(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(369, "Human-authored PR")
	server.addOpenPR(369, "feature/human-change", "main", "human-head", "main-base", false, nil, nil)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-369")
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/,goobers/docs-updater/")

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	var selected map[string]string
	data, err := os.ReadFile("selected-pr.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatal(err)
	}
	if selected["number"] != "369" || selected["advisoryMode"] != "true" {
		t.Fatalf("selected PR = %v, want #369 in advisory mode", selected)
	}
}

func TestPRSelectAlwaysHonorsNoMergeReviewLabel(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(370, "Opted-out PR")
	server.addOpenPR(370, "goobers/implementation/no-review", "main", "opt-out-head", "main-base", false, []string{noMergeReviewLabel}, []fakePRFile{{
		path: "internal/shared.go", status: "modified", additions: 6, deletions: 4,
		patch: "@@ -100,4 +100,6 @@",
	}})
	server.addIssue(371, "Foundation PR", "some-other-label")
	server.addOpenPR(371, "feature/foundation", "main", "foundation-head", "main-base", false, []string{"some-other-label"}, []fakePRFile{{
		path: "internal/shared.go", status: "modified", additions: 32, deletions: 1400,
		patch: "@@ -1,1400 +1,32 @@",
	}})
	server.setFileContent("foundation-head", "internal/shared.go", strings.Repeat("new\n", 32))
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-370")
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/,goobers/docs-updater/")
	t.Setenv(executor.InputEnvVar("excludeLabels"), "some-other-label")

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, filepath.Join(workDir, "selected-pr.json"))

	server.mu.Lock()
	issue := server.issues[370]
	pr := server.prs[370]
	server.mu.Unlock()
	if len(issue.comments) != 0 || len(issue.labels) != 0 ||
		len(pr.labels) != 1 || pr.labels[0] != noMergeReviewLabel {
		t.Fatalf("opted-out PR was mutated: issue labels=%v PR labels=%v comments=%v", issue.labels, pr.labels, issue.comments)
	}
}

func TestAuthorScopeAnyPreservesManagedSiblingSet(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Managed PR")
	server.addOpenPR(1, "goobers/implementation/managed", "main", "managed-head", "main-base", false, nil, []fakePRFile{{
		path: "internal/shared.go", status: "modified", additions: 2,
	}})
	server.addIssue(2, "Human PR")
	server.addOpenPR(2, "feature/human-change", "main", "human-head", "main-base", false, nil, []fakePRFile{{
		path: "internal/shared.go", status: "modified", additions: 2,
	}})
	server.addIssue(3, "Tutor PR")
	server.addOpenPR(3, "goobers/tutor/config-change", "main", "tutor-head", "main-base", false, nil, []fakePRFile{{
		path: "internal/shared.go", status: "modified", additions: 2,
	}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-managed")
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/,goobers/docs-updater/")

	selectDir := t.TempDir()
	t.Chdir(selectDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var selected map[string]string
	data, err := os.ReadFile(filepath.Join(selectDir, "selected-pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatal(err)
	}
	if selected["number"] != "1" || selected["advisoryMode"] != "false" {
		t.Fatalf("selected PR = %v, want managed PR #1 with automated handling", selected)
	}

	t.Setenv(executor.InputEnvVar("selectedNumber"), selected["number"])
	t.Setenv(executor.InputEnvVar("advisoryMode"), selected["advisoryMode"])
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var gathered struct {
		AdvisoryMode   string `json:"advisoryMode"`
		Siblings       []siblingPR
		Overlapping    []int  `json:"overlappingSiblings"`
		OverlappingCSV string `json:"overlappingSiblingsCsv"`
	}
	data, err = os.ReadFile("sibling-context.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &gathered); err != nil {
		t.Fatal(err)
	}
	if gathered.AdvisoryMode != "false" || len(gathered.Siblings) != 1 || gathered.Siblings[0].Number != 3 ||
		len(gathered.Overlapping) != 1 || gathered.Overlapping[0] != 3 || gathered.OverlappingCSV != "3" {
		t.Fatalf("managed sibling context = %+v, want tutor PR #3 retained and human PR #2 excluded", gathered)
	}
}

func TestMergeReviewOptOutAfterSelectionSuppressesVerdict(t *testing.T) {
	const (
		prNumber = 372
		runID    = "merge-review-372"
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Late opt-out PR")
	server.addOpenPR(prNumber, "feature/late-opt-out", "main", "late-head", "main-base", false, nil, nil)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/")

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	server.mu.Lock()
	server.prs[prNumber].labels = []string{noMergeReviewLabel}
	server.issues[prNumber].labels = []string{noMergeReviewLabel}
	server.mu.Unlock()

	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "must not be published",
		HeadSHA:  "late-head",
		BaseSHA:  "main-base",
	})
	t.Setenv(executor.InputEnvVar("selectedNumber"), "372")
	t.Setenv(executor.InputEnvVar("selectedHeadSha"), "late-head")
	t.Setenv(executor.InputEnvVar("selectedBaseSha"), "main-base")
	t.Setenv(executor.InputEnvVar("advisoryMode"), "true")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	var result map[string]string
	data, err := os.ReadFile("verdict-result.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["decision"] != "moot" || !strings.Contains(result["reason"], noMergeReviewLabel) {
		t.Fatalf("verdict result = %v, want opt-out moot result", result)
	}
	server.mu.Lock()
	comments := len(server.issues[prNumber].comments)
	reviews := len(server.prs[prNumber].reviews)
	server.mu.Unlock()
	if comments != 0 || reviews != 0 {
		t.Fatalf("late opt-out received feedback: comments=%d reviews=%d", comments, reviews)
	}
}

func TestMixedCompanyReviewPublishesAdvisoryWithoutRemediation(t *testing.T) {
	const (
		prNumber = 373
		runID    = "merge-review-373"
		headSHA  = "human-head"
		baseSHA  = "main-base"
	)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Human-authored PR")
	server.addOpenPR(prNumber, "feature/human-change", "main", headSHA, baseSHA, false, nil, []fakePRFile{{
		path: "internal/human.go", status: "modified", additions: 3,
	}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	routeMergeReviewTestRepo(t)
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/,goobers/docs-updater/")
	t.Setenv(executor.InputEnvVar("scopeDriftThreshold"), "0")
	t.Setenv(executor.InputEnvVar("scopeGateFilesThreshold"), "1")
	t.Setenv(executor.InputEnvVar("scopeGateLinesThreshold"), "1")

	selectDir := t.TempDir()
	t.Chdir(selectDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var selected map[string]string
	data, err := os.ReadFile(filepath.Join(selectDir, "selected-pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatal(err)
	}

	t.Setenv(executor.InputEnvVar("selectedNumber"), selected["number"])
	t.Setenv(executor.InputEnvVar("advisoryMode"), selected["advisoryMode"])
	gatherDir := t.TempDir()
	t.Chdir(gatherDir)
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var gathered struct {
		SelectedNumber  string `json:"selectedNumber"`
		SelectedHeadSHA string `json:"selectedHeadSha"`
		SelectedBaseSHA string `json:"selectedBaseSha"`
		AdvisoryMode    string `json:"advisoryMode"`
		ReviewDigest    string `json:"reviewDigest"`
		ScopeGateParked string `json:"scopeGateParked"`
	}
	data, err = os.ReadFile(filepath.Join(gatherDir, "sibling-context.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &gathered); err != nil {
		t.Fatal(err)
	}
	if gathered.SelectedNumber != "373" || gathered.SelectedHeadSHA != headSHA ||
		gathered.SelectedBaseSHA != baseSHA || gathered.AdvisoryMode != "true" ||
		gathered.ScopeGateParked != "false" {
		t.Fatalf("gathered context = %+v, want advisory PR #373 at the selected SHAs", gathered)
	}

	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Summary:   "Advisory findings",
		Rationale: "Please address the review findings.",
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityWarning,
			Class:    apiv1.FindingSubstantive,
			Message:  "A substantive concern",
		}},
	})
	t.Setenv(executor.InputEnvVar("selectedNumber"), gathered.SelectedNumber)
	t.Setenv(executor.InputEnvVar("selectedHeadSha"), gathered.SelectedHeadSHA)
	t.Setenv(executor.InputEnvVar("selectedBaseSha"), gathered.SelectedBaseSHA)
	t.Setenv(executor.InputEnvVar("reviewDigest"), gathered.ReviewDigest)
	t.Setenv(executor.InputEnvVar("advisoryMode"), gathered.AdvisoryMode)
	electDir := t.TempDir()
	t.Chdir(electDir)
	if code, stdout, stderr := runArgs(t, "elect-lander", root); code != 0 {
		t.Fatalf("elect-lander: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var election map[string]string
	data, err = os.ReadFile(filepath.Join(electDir, "election.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &election); err != nil {
		t.Fatal(err)
	}
	if election["elected"] != "false" || election["advisoryMode"] != "true" {
		t.Fatalf("election result = %v, advisory PR must never be elected", election)
	}

	applyDir := t.TempDir()
	t.Chdir(applyDir)
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	var result map[string]string
	data, err = os.ReadFile(filepath.Join(applyDir, "verdict-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["decision"] != "needs-changes" || result["advisoryMode"] != "true" ||
		result["priorityDispatchRequested"] != "false" {
		t.Fatalf("verdict result = %v, want advisory needs-changes without dispatch", result)
	}

	server.mu.Lock()
	issue := server.issues[prNumber]
	pr := server.prs[prNumber]
	server.mu.Unlock()
	if issue.state != "open" || len(issue.labels) != 0 || len(pr.labels) != 0 {
		t.Fatalf("advisory review remediated PR: issue state=%q issue labels=%v PR labels=%v", issue.state, issue.labels, pr.labels)
	}
	if len(pr.reviews) != 0 {
		t.Fatalf("native reviews = %+v, advisory feedback must remain non-blocking", pr.reviews)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("verdict comments = %v, want one advisory status comment", issue.comments)
	}
	posted, ok := parseVerdictComment(issue.comments[0])
	if !ok || posted.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("advisory verdict = %+v, ok = %t", posted, ok)
	}
}
