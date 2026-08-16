package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
)

func TestPRSelectSkipsUnchangedScopeGateParkedVerdict(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const baseSHA = "base-sha"

	server.addIssue(10, "Parked oversized PR", scopeGateLabel)
	server.addOpenPR(10, "goobers/implementation/parked", "main", "head-10", baseSHA, false, []string{scopeGateLabel}, nil)
	server.addComment(10, renderScopeGateStateComment(renderVerdictComment(apiv1.Verdict{
		Decision:    apiv1.VerdictPass,
		Digest:      computeReviewDigest("head-10", baseSHA, []string{scopeGateLabel}),
		SourceRunID: "prior-review",
		HeadSHA:     "head-10",
		BaseSHA:     baseSHA,
	}), true))

	for _, number := range []int{11, 12} {
		server.addIssue(number, "Eligible PR")
		server.addOpenPR(number, "goobers/implementation/eligible", "main", "head-"+strconv.Itoa(number), baseSHA, false, nil, nil)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-3133")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selected["number"] != "11" {
		t.Fatalf("selected PR = %q, want eligible PR #11 ahead of unchanged scope-gate parked PR #10", selected["number"])
	}
}

func TestPRSelectReconsidersScopeGateParkedPRAfterHeadChanges(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const baseSHA = "base-sha"

	server.addIssue(10, "Changed oversized PR", scopeGateLabel)
	server.addOpenPR(10, "goobers/implementation/parked", "main", "new-head", baseSHA, false, []string{scopeGateLabel}, nil)
	server.addComment(10, renderScopeGateStateComment(renderVerdictComment(apiv1.Verdict{
		Decision:    apiv1.VerdictPass,
		Digest:      computeReviewDigest("old-head", baseSHA, []string{scopeGateLabel}),
		SourceRunID: "prior-review",
		HeadSHA:     "old-head",
		BaseSHA:     baseSHA,
	}), true))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-3133-changed")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selected["number"] != "10" {
		t.Fatalf("selected PR = %q, want changed scope-gate PR #10 reconsidered", selected["number"])
	}
}

func TestPRSelectSkipsScopeGateParkedVerdictFromAdvisoryCycle(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const (
		runID   = "run-3133-advisory"
		baseSHA = "base-sha"
		headSHA = "head-10"
	)

	server.addIssue(10, "Parked advisory PR", scopeGateLabel)
	server.addOpenPR(10, "feature/parked", "main", headSHA, baseSHA, false, []string{scopeGateLabel}, nil)
	server.addIssue(11, "Eligible advisory PR")
	server.addOpenPR(11, "feature/eligible", "main", "head-11", baseSHA, false, nil, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	routeMergeReviewTestRepo(t)
	digest := computeReviewDigest(headSHA, baseSHA, []string{scopeGateLabel})
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Digest:   digest,
		HeadSHA:  headSHA,
		BaseSHA:  baseSHA,
	})
	t.Setenv(executor.InputEnvVar("selectedNumber"), "10")
	t.Setenv(executor.InputEnvVar("reviewDigest"), digest)
	t.Setenv(executor.InputEnvVar("advisoryMode"), "true")
	t.Setenv(executor.InputEnvVar("scopeGateParked"), "true")

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/")
	selectDir := t.TempDir()
	t.Chdir(selectDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(selectDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selected["number"] != "11" {
		t.Fatalf("selected PR = %q, want eligible PR #11 after advisory cycle parked PR #10", selected["number"])
	}
}
