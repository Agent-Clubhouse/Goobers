package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// seedGateVerdictJournal writes a run journal whose "review" gate already
// evaluated with the given Verdict recorded as an artifact — what
// internal/gate's recordVerdict produces for a real agentic gate evaluation
// (issue #358's schema, reused as-is for merge-review's holistic Verdict) —
// so apply-verdict's readLatestGateVerdict can recover it the way the live
// runner path does, without needing a real (or fake) reviewer goober in this
// CLI-level test.
func seedGateVerdictJournal(t *testing.T, root, runID string, v apiv1.Verdict) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "merge-review", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	ref, err := run.RecordArtifact("verdict/review-1.json", data)
	if err != nil {
		t.Fatalf("record verdict artifact: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated,
		Gate: "review",
		Name: "verdict/review-1.json",
		Ref:  &ref,
	}); err != nil {
		t.Fatalf("seed gate.evaluated: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close seeded journal: %v", err)
	}
}

// TestMergeReviewNamesCrossPRConflict is issue #359's own test-plan
// acceptance item: "two overlapping fixture PRs -> the verdict on one names
// the cross-PR conflict with the other." It drives the real pr-select ->
// gather-sibling-context -> apply-verdict CLI chain (each a real `goobers`
// subcommand invocation, mirroring prchainfinish241_test.go's convention for
// provider-backed deterministic stages) against a fake GitHub server seeded
// with two open PRs that touch the same file. The middle "review" gate
// itself is agentic (a real reviewer goober judging the diff), which this
// CLI-level test cannot exercise without a real or fake harness process —
// consistent with acceptance_test.go's scoping note that provider-backed OS
// subprocesses and the agentic loop are tested at their own layers, not
// force-chained into one mega run. What this test proves instead: (1) the
// sibling context gather-sibling-context hands the reviewer actually
// contains the overlapping file (the data a real reviewer needs to name the
// conflict), and (2) a Verdict that names it flows end-to-end through
// apply-verdict into the selected PR's real label + comment — the two halves
// of the "verdict names the cross-PR conflict" contract that are this
// binary's job to prove, independent of the reviewer's own judgment quality.
func TestMergeReviewNamesCrossPRConflict(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	const (
		selectedNumber = 10
		siblingNumber  = 11
		overlapFile    = "internal/runner/run.go"
	)
	server.addIssue(selectedNumber, "Selected PR")
	server.addOpenPR(selectedNumber, "goobers/implementation/run-10", "main", "sha10head", "shamainbase",
		false, nil, []fakePRFile{{path: overlapFile, status: "modified", additions: 5, deletions: 1}})
	server.addOpenPR(siblingNumber, "goobers/implementation/run-11", "main", "sha11head", "shamainbase",
		false, nil, []fakePRFile{{path: overlapFile, status: "modified", additions: 2, deletions: 0}})

	const runID = "run-1"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)

	// pr-select: both PRs are open, non-draft, green-CI, and unlabeled —
	// picks the lowest number (#10).
	selectDir := t.TempDir()
	t.Chdir(selectDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	selData, err := os.ReadFile(filepath.Join(selectDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selected map[string]string
	if err := json.Unmarshal(selData, &selected); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selected["number"] != "10" {
		t.Fatalf("selected number = %q, want 10", selected["number"])
	}

	// gather-sibling-context: PR #11's files (the OTHER open PR) must include
	// the same overlapFile PR #10 touches — the cross-PR conflict signal a
	// real reviewer would act on.
	siblingDir := t.TempDir()
	t.Chdir(siblingDir)
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", selected["number"])
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	ctxData, err := os.ReadFile(filepath.Join(siblingDir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var ctx struct {
		SelectedNumber  string `json:"selectedNumber"` // string end-to-end (#413)
		SelectedHeadSha string `json:"selectedHeadSha"`
		SelectedBaseSha string `json:"selectedBaseSha"`
		Siblings        []struct {
			Number int      `json:"number"`
			Files  []string `json:"files"`
		} `json:"siblings"`
	}
	if err := json.Unmarshal(ctxData, &ctx); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	if ctx.SelectedHeadSha != "sha10head" || ctx.SelectedBaseSha != "shamainbase" {
		t.Fatalf("selected SHAs = (%q, %q), want (sha10head, shamainbase)", ctx.SelectedHeadSha, ctx.SelectedBaseSha)
	}
	if len(ctx.Siblings) != 1 || ctx.Siblings[0].Number != siblingNumber {
		t.Fatalf("siblings = %+v, want exactly PR #%d", ctx.Siblings, siblingNumber)
	}
	foundOverlap := false
	for _, f := range ctx.Siblings[0].Files {
		if f == overlapFile {
			foundOverlap = true
		}
	}
	if !foundOverlap {
		t.Fatalf("sibling #%d files = %v, want to include the overlapping file %q", siblingNumber, ctx.Siblings[0].Files, overlapFile)
	}

	// Seed the review gate's Verdict as if a reviewer had just evaluated the
	// context above and named the conflict with PR #11.
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: "cross-PR conflict must be resolved before merge",
		HeadSHA:   ctx.SelectedHeadSha,
		BaseSHA:   ctx.SelectedBaseSha,
		Findings: []apiv1.Finding{
			{
				Severity: apiv1.SeverityWarning,
				Class:    apiv1.FindingConflict,
				Message:  "overlaps open PR #11 on internal/runner/run.go",
			},
		},
	})

	// apply-verdict: the verdict's SHA pin still matches the PR's current
	// state, so it applies — the label + a comment naming PR #11.
	applyDir := t.TempDir()
	t.Chdir(applyDir)
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	issue := server.issues[selectedNumber]
	server.mu.Unlock()
	if issue == nil {
		t.Fatal("selected PR's issue record vanished")
	}
	hasLabel := false
	for _, l := range issue.labels {
		if l == "goobers:needs-remediation" {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Fatalf("labels = %v, want goobers:needs-remediation", issue.labels)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("comments = %v, want exactly 1", issue.comments)
	}
	if !strings.Contains(issue.comments[0], "#11") {
		t.Fatalf("verdict comment = %q, want it to name PR #11", issue.comments[0])
	}
}
