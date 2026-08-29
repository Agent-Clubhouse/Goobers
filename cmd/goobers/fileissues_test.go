package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/flake"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/nomination"
	"github.com/goobers/goobers/providers"
)

// These tests drive `goobers file-issues` through the real CLI entrypoint
// against the fake GitHub server, at the seam the change crosses: the
// artifact on disk in, provider HTTP calls out. Labels are asserted on the
// fake server's issue records, never on the command's own summary.

const (
	fileIssuesTestRunID    = "run-nom-1"
	fileIssuesTestPartLbl  = "goobers:cloud"
	fileIssuesForbiddenLbl = "goobers:ready,goobers:critical,goobers:claimed,goobers:blocked-on-sibling,goobers:local,ci:flake"
)

type fileIssuesFixture struct {
	t          *testing.T
	root       string
	workspace  string
	server     *fakeGitHubServer
	resultFile string
}

// newFileIssuesFixture stands up an empty instance root, a fake GitHub
// server, a scratch workspace as the process cwd (the stage worktree), and
// the run/repo/credential env the runner would inject. The approve
// credential and autoApprove input are left to each test.
func newFileIssuesFixture(t *testing.T) *fileIssuesFixture {
	t.Helper()
	f := &fileIssuesFixture{t: t, root: t.TempDir(), workspace: t.TempDir()}
	f.server = newFakeGitHubServer(t, "your-org", "your-repo")
	previous := newGitHubProvider
	newGitHubProvider = f.server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })
	t.Chdir(f.workspace)

	t.Setenv("GOOBERS_RUN_ID", fileIssuesTestRunID)
	t.Setenv("GOOBERS_WORKFLOW", "defect-nomination")
	t.Setenv(executor.InstanceRootEnvVar, f.root)
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "write-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "read-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_APPROVE", "")
	t.Setenv(executor.InputEnvVar("partitionLabel"), fileIssuesTestPartLbl)
	t.Setenv(executor.InputEnvVar("autoApprove"), "")
	t.Setenv(executor.InputEnvVar("maxPerRun"), "")
	t.Setenv(executor.InputEnvVar("dedupeWindowDays"), "")
	f.resultFile = filepath.Join(t.TempDir(), fileIssuesResultFileName)
	t.Setenv(executor.InputEnvVar("resultFile"), f.resultFile)
	return f
}

// verifiedArtifact writes a stage artifact into the workspace and returns
// the artifact evidence pointer the publisher can verify against it.
func (f *fileIssuesFixture) verifiedArtifact(name, content string) nomination.Evidence {
	f.t.Helper()
	path := filepath.Join("artifacts", name)
	if err := os.MkdirAll(filepath.Join(f.workspace, "artifacts"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, path), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return nomination.Evidence{Kind: nomination.EvidenceArtifact, Path: filepath.ToSlash(path), Digest: "sha256:" + hex.EncodeToString(sum[:])}
}

// approvable is a nomination that clears every auto-approval precondition
// once the stage opts in and the approve credential resolves.
func (f *fileIssuesFixture) approvable(key string) nomination.Nomination {
	f.t.Helper()
	return nomination.Nomination{
		Key: key, DedupeKey: "vet:internal/worktree:" + key,
		Title:      "go vet: unused result in internal/worktree (" + key + ")",
		Body:       "go vet reports an unchecked error return in internal/worktree/manager.go; the result of Close is dropped.",
		Labels:     []string{"area:runner", "type:bug"},
		RiskClass:  nomination.RiskLow,
		RiskReason: "a vet finding with a source location and an artifact-backed stack",
		Evidence: []nomination.Evidence{
			f.verifiedArtifact(key+"-vet.txt", "internal/worktree/manager.go:88: result of Close is not used ("+key+")"),
			{Kind: nomination.EvidenceSource, Path: "internal/worktree/manager.go", Line: 88},
		},
	}
}

func (f *fileIssuesFixture) writeArtifact(nominations ...nomination.Nomination) nomination.Artifact {
	f.t.Helper()
	artifact := nomination.Artifact{
		Schema: nomination.SchemaV1, RunID: fileIssuesTestRunID,
		Producer: nomination.Producer{Stage: "triage", Attempt: 1}, Nominations: nominations,
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, fileIssuesArtifactName), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return artifact
}

func (f *fileIssuesFixture) enableAutoApprove(withCredential bool) {
	f.t.Helper()
	f.t.Setenv(executor.InputEnvVar("autoApprove"), "low-risk-only")
	if withCredential {
		f.t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_APPROVE", "approve-token")
	}
}

func (f *fileIssuesFixture) run(args ...string) (int, string, string) {
	f.t.Helper()
	return runArgs(f.t, append([]string{"file-issues"}, args...)...)
}

func (f *fileIssuesFixture) mustRun(args ...string) fileIssuesResult {
	f.t.Helper()
	code, stdout, stderr := f.run(args...)
	if code != 0 {
		f.t.Fatalf("file-issues %v: code = %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	var result fileIssuesResult
	data, err := os.ReadFile(f.resultFile)
	if err != nil {
		f.t.Fatalf("read result: %v", err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		f.t.Fatalf("parse result %s: %v", data, err)
	}
	return result
}

func (f *fileIssuesFixture) mustCheck() fileIssuesCheckResult {
	f.t.Helper()
	code, stdout, stderr := f.run("--check")
	if code != 0 {
		f.t.Fatalf("file-issues --check: code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var result fileIssuesCheckResult
	data, err := os.ReadFile(f.resultFile)
	if err != nil {
		f.t.Fatalf("read check result: %v", err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		f.t.Fatalf("parse check result %s: %v", data, err)
	}
	return result
}

func (f *fileIssuesFixture) issueCount() int {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return len(f.server.issues)
}

func (f *fileIssuesFixture) issueBody(number int) string {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return f.server.issues[number].body
}

func (f *fileIssuesFixture) issueComments(number int) []string {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]string(nil), f.server.issues[number].comments...)
}

func (f *fileIssuesFixture) issueNumber(id string) int {
	f.t.Helper()
	n, err := strconv.Atoi(id)
	if err != nil {
		f.t.Fatalf("issue id %q: %v", id, err)
	}
	return n
}

func flakeFingerprintFor(tf *nomination.TestFailure) string {
	return flake.Fingerprint(tf.Package, tf.Test, flake.NormalizeSignature(tf.Signature))
}

// seedFiledIssue plants an issue exactly as the publisher would have written
// it — key marker, attribution to runID, and the provider's run-id footer —
// so a test can model a prior run, a prior attempt, or a closed duplicate.
func (f *fileIssuesFixture) seedFiledIssue(number int, artifactRunID string, n nomination.Nomination, labels ...string) {
	f.t.Helper()
	hash := nomination.KeyHash(n.DedupeKey)
	artifact := nomination.Artifact{RunID: artifactRunID, Producer: nomination.Producer{Stage: "triage", Attempt: 1}}
	body := nomination.IssueBody(artifact, hash, n, false) + "\n\n---\ngoobers run-id: " + nomination.CreateRunID(hash, artifactRunID)
	f.server.addIssue(number, n.Title, labels...)
	f.server.mu.Lock()
	f.server.issues[number].body = body
	f.server.mu.Unlock()
}

func assertNoForbiddenLabels(t *testing.T, labels []string) {
	t.Helper()
	for _, forbidden := range strings.Split(fileIssuesForbiddenLbl, ",") {
		if slices.Contains(labels, forbidden) {
			t.Fatalf("labels %v carry forbidden label %q", labels, forbidden)
		}
	}
}

func TestFileIssuesCheckValidatesScansAndMutatesNothing(t *testing.T) {
	f := newFileIssuesFixture(t)
	existing := f.approvable("already-open")
	f.seedFiledIssue(7, "run-old", existing, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.writeArtifact(existing, f.approvable("fresh"))
	before := f.issueCount()

	check := f.mustCheck()
	if !check.Valid || check.FiledCount != 1 || check.SuppressedCount != 1 || check.OverBudget != 0 || check.NominationsDigest == "" {
		t.Fatalf("check = %+v; want valid, 1 to file, 1 suppressed", check)
	}
	if f.issueCount() != before {
		t.Fatalf("--check created issues: %d -> %d", before, f.issueCount())
	}
	if _, err := os.Stat(filepath.Join(f.workspace, mutationsSidecarFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--check recorded mutations: %v", err)
	}
	if got := f.issueComments(7); len(got) != 0 {
		t.Fatalf("--check commented on the open duplicate: %v", got)
	}

	// The read-only mode uses the exact read credential, never the write one.
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "")
	if code, _, stderr := f.run("--check"); code == 0 || !strings.Contains(stderr, "GOOBERS_CRED_GITHUB_ISSUES_READ") {
		t.Fatalf("--check without the read credential: code = %d, stderr = %q", code, stderr)
	}
}

func TestFileIssuesCheckReportsInvalidArtifactWithoutFiling(t *testing.T) {
	f := newFileIssuesFixture(t)
	forged := f.approvable("forged")
	forged.Labels = append(forged.Labels, providers.LabelApproved)
	forged.Body += "\n<!-- goobers-nomination-key:" + strings.Repeat("0", 64) + " -->"
	f.writeArtifact(forged)

	check := f.mustCheck()
	if check.Valid || len(check.Errors) < 2 {
		t.Fatalf("check = %+v; want invalid with the label and marker findings", check)
	}
	joined := strings.Join(check.Errors, "\n")
	if !strings.Contains(joined, `publisher-owned label "goobers:approved"`) || !strings.Contains(joined, "control marker") {
		t.Fatalf("errors = %v", check.Errors)
	}
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "refusing to file an invalid nominations artifact") {
		t.Fatalf("write over an invalid artifact: code = %d, stderr = %q", code, stderr)
	}
	if f.issueCount() != 0 {
		t.Fatalf("invalid artifact filed %d issue(s)", f.issueCount())
	}
}

func TestFileIssuesRequiresPartitionLabel(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.writeArtifact(f.approvable("one"))
	t.Setenv(executor.InputEnvVar("partitionLabel"), "")
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "partitionLabel input is required") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if f.issueCount() != 0 {
		t.Fatal("filed without a partition label")
	}
}

// TestFileIssuesLabelPolicy pins the closed label set the publisher applies
// for every risk class, with and without the approve opt-in and credential.
func TestFileIssuesLabelPolicy(t *testing.T) {
	base := []string{"goobers", fileIssuesTestPartLbl, providers.LabelNominated, "area:runner", "type:bug"}
	cases := []struct {
		name        string
		risk        nomination.RiskClass
		humanReview bool
		autoApprove bool
		credential  bool
		want        []string
	}{
		{name: "standard files unapproved", risk: nomination.RiskStandard, autoApprove: true, credential: true, want: base},
		{name: "human files needs-human", risk: nomination.RiskHuman, autoApprove: true, credential: true, want: append(slices.Clone(base), providers.LabelNeedsHuman)},
		{name: "finding requiring human review files needs-human", risk: nomination.RiskLow, humanReview: true, autoApprove: true, credential: true, want: append(slices.Clone(base), providers.LabelNeedsHuman)},
		{name: "low without opt-in files unapproved", risk: nomination.RiskLow, want: base},
		{name: "low with opt-in but no credential files unapproved", risk: nomination.RiskLow, autoApprove: true, want: base},
		{name: "low with opt-in and credential is approved", risk: nomination.RiskLow, autoApprove: true, credential: true, want: append(slices.Clone(base), providers.LabelApproved)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFileIssuesFixture(t)
			if tc.autoApprove {
				f.enableAutoApprove(tc.credential)
			}
			n := f.approvable("policy")
			n.RiskClass = tc.risk
			n.RequiresHumanReview = tc.humanReview
			f.writeArtifact(n)

			result := f.mustRun()
			if result.Created != 1 || len(result.Issues) != 1 {
				t.Fatalf("result = %+v; want one created issue", result)
			}
			got := f.server.issueLabels(f.issueNumber(result.Issues[0].IssueID))
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("labels = %v, want %v", got, want)
			}
			assertNoForbiddenLabels(t, got)
			if approved := slices.Contains(got, providers.LabelApproved); approved != (result.Approved == 1) {
				t.Fatalf("result approved = %d, issue approved = %v", result.Approved, approved)
			}
			body := f.issueBody(f.issueNumber(result.Issues[0].IssueID))
			if _, ok := nomination.ParseKeyMarker(body); !ok || !strings.HasPrefix(body, "<!-- goobers-nomination-key:") {
				t.Fatalf("body does not start with the key marker:\n%s", body)
			}
			if wantHuman := slices.Contains(want, providers.LabelNeedsHuman); wantHuman != strings.Contains(body, "For the human:") {
				t.Fatalf("For the human block present = %v, want %v:\n%s", !wantHuman, wantHuman, body)
			}
		})
	}
}

// TestFileIssuesApprovalPreconditions ablates the eight auto-approval
// preconditions one at a time: each must file the issue unapproved and name
// the unmet precondition — never skip it and never error.
func TestFileIssuesApprovalPreconditions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fileIssuesFixture, n *nomination.Nomination)
		want  string
	}{
		{"1 riskClass not low", func(_ *fileIssuesFixture, n *nomination.Nomination) { n.RiskClass = nomination.RiskStandard }, "not low"},
		{"2 autoApprove not enabled", func(f *fileIssuesFixture, _ *nomination.Nomination) {
			f.t.Setenv(executor.InputEnvVar("autoApprove"), "never")
		}, "autoApprove is not enabled"},
		{"3 approve credential missing", func(f *fileIssuesFixture, _ *nomination.Nomination) {
			f.t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_APPROVE", "")
		}, "credential did not resolve"},
		{"4 no verifiable evidence", func(_ *fileIssuesFixture, n *nomination.Nomination) {
			n.Evidence[0].Digest = "sha256:" + strings.Repeat("f", 64)
		}, "no evidence pointer could be verified"},
		{"5 source evidence spans directories", func(_ *fileIssuesFixture, n *nomination.Nomination) {
			n.Evidence = append(n.Evidence, nomination.Evidence{Kind: nomination.EvidenceSource, Path: "internal/runner/run.go", Line: 1})
		}, "exactly one directory"},
		{"6 load-bearing path", func(_ *fileIssuesFixture, n *nomination.Nomination) {
			n.Evidence[1].Path = "api/v1alpha1/workflow_types.go"
		}, "load-bearing path"},
		{"7 needs-human trigger", func(_ *fileIssuesFixture, n *nomination.Nomination) { n.RequiresHumanReview = true }, "requires human review"},
		{"7b type:feature proposes new behaviour", func(_ *fileIssuesFixture, n *nomination.Nomination) {
			n.Labels = []string{"area:runner", "type:feature"}
		}, "type:feature"},
		{"8 prior issue with the same key", func(f *fileIssuesFixture, n *nomination.Nomination) {
			f.seedFiledIssue(3, "run-old", *n, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
			f.server.closeIssue(3)
			f.server.setIssueUpdatedAt(3, time.Now().Add(-60*24*time.Hour))
		}, "prior issue carried the same nomination key: #3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFileIssuesFixture(t)
			f.enableAutoApprove(true)
			n := f.approvable("bar")
			tc.setup(f, &n)
			f.writeArtifact(n)

			result := f.mustRun()
			if result.Created != 1 || len(result.Issues) != 1 {
				t.Fatalf("result = %+v; want exactly one created issue", result)
			}
			if result.Approved != 0 || result.Issues[0].Approved {
				t.Fatalf("issue was approved despite %s: %+v", tc.name, result.Issues[0])
			}
			if !strings.Contains(strings.Join(result.Issues[0].ApprovalUnmet, "\n"), tc.want) {
				t.Fatalf("approvalUnmet = %v, want one containing %q", result.Issues[0].ApprovalUnmet, tc.want)
			}
			labels := f.server.issueLabels(f.issueNumber(result.Issues[0].IssueID))
			if slices.Contains(labels, providers.LabelApproved) {
				t.Fatalf("issue carries goobers:approved: %v", labels)
			}
		})
	}

	t.Run("all eight hold", func(t *testing.T) {
		f := newFileIssuesFixture(t)
		f.enableAutoApprove(true)
		f.writeArtifact(f.approvable("bar"))
		result := f.mustRun()
		if result.Created != 1 || result.Approved != 1 || len(result.Issues[0].ApprovalUnmet) != 0 {
			t.Fatalf("result = %+v; want one approved issue", result)
		}
		if !f.server.issueHasLabel(f.issueNumber(result.Issues[0].IssueID), providers.LabelApproved) {
			t.Fatal("issue does not carry goobers:approved")
		}
	})
}

func TestFileIssuesJournalEvidenceVerifies(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.enableAutoApprove(true)
	run, err := journal.Create(layoutFor(f.root).RunsDir(), journal.RunIdentity{RunID: fileIssuesTestRunID, Workflow: "defect-nomination", Gaggle: "goobers"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "collect", Attempt: 1, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(layoutFor(f.root).RunsDir(), fileIssuesTestRunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %d, %v", len(events), err)
	}
	last := events[len(events)-1].Seq

	n := f.approvable("journal")
	n.Evidence = []nomination.Evidence{
		{Kind: nomination.EvidenceJournal, RunID: fileIssuesTestRunID, Seq: last},
		{Kind: nomination.EvidenceSource, Path: "internal/worktree/manager.go", Line: 88},
	}
	bad := f.approvable("journal-bad")
	bad.Evidence = []nomination.Evidence{
		{Kind: nomination.EvidenceJournal, RunID: fileIssuesTestRunID, Seq: last + 100},
		{Kind: nomination.EvidenceSource, Path: "internal/worktree/manager.go", Line: 88},
	}
	f.writeArtifact(n, bad)
	result := f.mustRun()
	if result.Created != 2 || result.Approved != 1 {
		t.Fatalf("result = %+v; want the real journal pointer approved and the phantom seq unapproved", result)
	}
	for _, issue := range result.Issues {
		switch issue.Key {
		case "journal":
			if !issue.Approved {
				t.Fatalf("a real journal pointer did not verify: %v", issue.ApprovalUnmet)
			}
		case "journal-bad":
			if issue.Approved || !strings.Contains(strings.Join(issue.ApprovalUnmet, "\n"), "no evidence pointer could be verified") {
				t.Fatalf("a non-existent journal seq verified: %+v", issue)
			}
		}
	}
}

func TestFileIssuesDedupeOpenAlwaysClosedWithinWindow(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.enableAutoApprove(true)
	openDup := f.approvable("open-dup")
	closedRecent := f.approvable("closed-recent")
	closedOld := f.approvable("closed-old")
	f.seedFiledIssue(10, "run-old", openDup, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.setIssueUpdatedAt(10, time.Now().Add(-400*24*time.Hour)) // age never matters for an open match
	f.seedFiledIssue(11, "run-old", closedRecent, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.closeIssue(11)
	f.server.setIssueUpdatedAt(11, time.Now().Add(-5*24*time.Hour))
	f.seedFiledIssue(12, "run-old", closedOld, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.closeIssue(12)
	f.server.setIssueUpdatedAt(12, time.Now().Add(-40*24*time.Hour))
	f.writeArtifact(openDup, closedRecent, closedOld)

	result := f.mustRun()
	if result.Created != 1 || result.Suppressed != 2 || len(result.Issues) != 1 || result.Issues[0].Key != "closed-old" {
		t.Fatalf("result = %+v; want closed-old filed and the other two suppressed", result)
	}
	if result.Approved != 0 || !strings.Contains(strings.Join(result.Issues[0].ApprovalUnmet, "\n"), "#12") {
		t.Fatalf("a re-filed key was approved despite its prior issue: %+v", result.Issues[0])
	}
	reasons := map[string]string{}
	for _, s := range result.Suppressions {
		reasons[s.Key] = s.Reason
	}
	if !strings.Contains(reasons["open-dup"], "open issue #10") || !strings.Contains(reasons["closed-recent"], "#11") || !strings.Contains(reasons["closed-recent"], "dedupe window") {
		t.Fatalf("suppression reasons = %v", reasons)
	}
	if f.issueCount() != 4 {
		t.Fatalf("issue count = %d, want 4", f.issueCount())
	}
	// The open duplicate gets one occurrence comment per run, and only one.
	comments := f.issueComments(10)
	if len(comments) != 1 || !strings.Contains(comments[0], nomination.SeenMarker(nomination.KeyHash(openDup.DedupeKey), fileIssuesTestRunID)) {
		t.Fatalf("occurrence comments on #10 = %v", comments)
	}
	if got := f.issueComments(11); len(got) != 0 {
		t.Fatalf("closed duplicate was annotated: %v", got)
	}
	if again := f.mustRun(); again.Created != 0 || len(f.issueComments(10)) != 1 {
		t.Fatalf("rerun: created = %d, comments on #10 = %v", again.Created, f.issueComments(10))
	}

	// A wider window suppresses closed-old too; a zero window suppresses
	// only the open match.
	t.Setenv(executor.InputEnvVar("dedupeWindowDays"), "60")
	t.Setenv("GOOBERS_RUN_ID", "run-nom-3")
	f.server.closeIssue(f.issueNumber(result.Issues[0].IssueID))
	f.server.setIssueUpdatedAt(f.issueNumber(result.Issues[0].IssueID), time.Now().Add(-50*24*time.Hour))
	if wide := f.mustRun(); wide.Created != 0 || wide.Suppressed != 3 {
		t.Fatalf("60-day window: %+v", wide)
	}
	t.Setenv(executor.InputEnvVar("dedupeWindowDays"), "0")
	t.Setenv("GOOBERS_RUN_ID", "run-nom-4")
	if none := f.mustRun(); none.Created != 2 || none.Suppressed != 1 {
		t.Fatalf("zero-day window: %+v", none)
	}
}

func TestFileIssuesBudgetOrdersByEvidenceAndOverflows(t *testing.T) {
	f := newFileIssuesFixture(t)
	sourceOnly := f.approvable("z-source-only")
	sourceOnly.Evidence = sourceOnly.Evidence[1:]
	journalBacked := f.approvable("y-journal")
	journalBacked.Evidence = []nomination.Evidence{{Kind: nomination.EvidenceJournal, RunID: "run-x", Seq: 4}, journalBacked.Evidence[1]}
	artifactA := f.approvable("b-artifact")
	artifactB := f.approvable("a-artifact")
	weakest := f.approvable("w-source-only")
	weakest.Evidence = weakest.Evidence[1:]
	f.writeArtifact(sourceOnly, weakest, journalBacked, artifactA, artifactB)

	result := f.mustRun()
	if result.Created != 3 || result.Overflow != 2 || len(result.Issues) != 3 {
		t.Fatalf("result = %+v; want 3 created and 2 over budget", result)
	}
	var keys []string
	for _, issue := range result.Issues {
		keys = append(keys, issue.Key)
	}
	if want := []string{"a-artifact", "b-artifact", "y-journal"}; !slices.Equal(keys, want) {
		t.Fatalf("filed order = %v, want %v (artifact > journal > source, then key)", keys, want)
	}
	if want := []string{"w-source-only", "z-source-only"}; !slices.Equal(result.OverflowKeys, want) {
		t.Fatalf("overflow = %v, want %v", result.OverflowKeys, want)
	}

	t.Setenv(executor.InputEnvVar("maxPerRun"), "1")
	t.Setenv("GOOBERS_RUN_ID", "run-next")
	next := f.mustRun()
	if next.Created != 1 || next.Suppressed != 3 || next.Overflow != 1 || next.Issues[0].Key != "w-source-only" {
		t.Fatalf("next cycle = %+v; want the overflow drained one at a time", next)
	}
}

func TestFileIssuesRetryIsIdempotentAndResumesMidBatch(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.enableAutoApprove(true)
	first, second, third := f.approvable("first"), f.approvable("second"), f.approvable("third")
	// Attempt 1 created `second` (labels, marker, run-id footer) and then died.
	f.seedFiledIssue(5, fileIssuesTestRunID, second, "goobers", fileIssuesTestPartLbl, providers.LabelNominated, "area:runner", "type:bug")
	f.writeArtifact(first, second, third)

	resumed := f.mustRun()
	if resumed.Created != 2 || resumed.Filed != 3 || resumed.Suppressed != 0 {
		t.Fatalf("resumed attempt = %+v; want only the missing remainder created", resumed)
	}
	for _, issue := range resumed.Issues {
		if issue.Key == "second" && (issue.IssueID != "5" || !issue.Reused) {
			t.Fatalf("second was not resumed onto #5: %+v", issue)
		}
		if !issue.Approved {
			t.Fatalf("%s not approved: %v", issue.Key, issue.ApprovalUnmet)
		}
	}
	if f.issueCount() != 3 {
		t.Fatalf("issue count = %d, want 3", f.issueCount())
	}

	// A full retry of the same attempt creates nothing and changes nothing.
	labelsBefore := map[int][]string{}
	for n := 5; n <= 7; n++ {
		labelsBefore[n] = f.server.issueLabels(n)
	}
	retried := f.mustRun()
	if retried.Created != 0 || retried.Filed != 3 || retried.Approved != 3 || f.issueCount() != 3 {
		t.Fatalf("retry = %+v, issues = %d; want nothing new", retried, f.issueCount())
	}
	for n, before := range labelsBefore {
		if after := f.server.issueLabels(n); !slices.Equal(before, after) {
			t.Fatalf("#%d labels changed on retry: %v -> %v", n, before, after)
		}
		if got := f.issueComments(n); len(got) != 0 {
			t.Fatalf("#%d gained comments on retry: %v", n, got)
		}
	}

	// A later run sees three open duplicates: creates 0, suppresses 3.
	t.Setenv("GOOBERS_RUN_ID", "run-tomorrow")
	later := f.mustRun()
	if later.Created != 0 || later.Suppressed != 3 || f.issueCount() != 3 {
		t.Fatalf("next-day run = %+v, issues = %d", later, f.issueCount())
	}
}

func TestFileIssuesNeverLabelsFlakeWatchIssues(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.enableAutoApprove(true)

	// A flake-watch issue whose fingerprint matches the nomination's test
	// failure suppresses it outright.
	fingerprinted := f.approvable("flaky-test")
	fingerprinted.TestFailure = &nomination.TestFailure{
		Package: "github.com/goobers/goobers/internal/runner", Test: "TestRunnerRace",
		Signature: "run_test.go:41: expected 3 got 2\n    goroutine 17 [running]:",
	}
	f.server.addIssue(20, "[flake] TestRunnerRace", nomination.FlakeLabel)
	f.server.mu.Lock()
	f.server.issues[20].body = "<!-- goobers-flake-fingerprint:" + flakeFingerprintFor(fingerprinted.TestFailure) + " -->\nowned by flake-watch"
	f.server.mu.Unlock()

	// An issue that carries ci:flake AND the nomination's run-id footer (the
	// retry lookup finds it) must not receive a single goobers label.
	adopted := f.approvable("adopted-by-flake-watch")
	f.seedFiledIssue(21, fileIssuesTestRunID, adopted, nomination.FlakeLabel)

	f.writeArtifact(fingerprinted, adopted)
	result := f.mustRun()
	if result.Suppressed != 1 || result.Refused != 1 || result.Created != 0 || result.Filed != 0 {
		t.Fatalf("result = %+v; want one suppressed and one refused", result)
	}
	if !strings.Contains(result.Suppressions[0].Reason, "flake-watch already owns fingerprint") || result.Suppressions[0].IssueID != "20" {
		t.Fatalf("suppression = %+v", result.Suppressions[0])
	}
	if result.Refusals[0].IssueID != "21" || !strings.Contains(result.Refusals[0].Reason, nomination.FlakeLabel) {
		t.Fatalf("refusal = %+v", result.Refusals[0])
	}
	for _, n := range []int{20, 21} {
		if got := f.server.issueLabels(n); !slices.Equal(got, []string{nomination.FlakeLabel}) {
			t.Fatalf("#%d labels = %v; want only ci:flake", n, got)
		}
	}
	if f.issueCount() != 2 {
		t.Fatalf("issue count = %d, want 2", f.issueCount())
	}
}

func TestFileIssuesRefusesArtifactTheCheckDidNotMarkValid(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.writeArtifact(f.approvable("bound"))
	check := f.mustCheck()
	checkFile := filepath.Join(f.workspace, fileIssuesCheckFileName)
	data, _ := json.Marshal(check)
	if err := os.WriteFile(checkFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if result := f.mustRun(); result.Created != 1 {
		t.Fatalf("bound write = %+v", result)
	}

	stale := check
	stale.NominationsDigest = "sha256:" + strings.Repeat("0", 64)
	data, _ = json.Marshal(stale)
	if err := os.WriteFile(checkFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f.writeArtifact(f.approvable("rebound"))
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "do not match the artifact file-issues --check marked valid") {
		t.Fatalf("digest mismatch: code = %d, stderr = %q", code, stderr)
	}
	invalid := check
	invalid.Valid = false
	data, _ = json.Marshal(invalid)
	if err := os.WriteFile(checkFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "did not mark valid") {
		t.Fatalf("invalid check: code = %d, stderr = %q", code, stderr)
	}
	if f.issueCount() != 1 {
		t.Fatalf("issue count = %d, want 1", f.issueCount())
	}
}

func TestFileIssuesRecordsEveryMutationInTheSidecar(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.enableAutoApprove(true)
	f.writeArtifact(f.approvable("recorded"))
	result := f.mustRun()
	data, err := os.ReadFile(filepath.Join(f.workspace, mutationsSidecarFile))
	if err != nil {
		t.Fatalf("read mutation sidecar: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var fact mutationFact
		if err := json.Unmarshal([]byte(line), &fact); err != nil {
			t.Fatalf("parse sidecar line %q: %v", line, err)
		}
		if fact.Kind != "issue" || fact.ID != result.Issues[0].IssueID {
			t.Fatalf("sidecar fact %+v does not name the filed issue %s", fact, result.Issues[0].IssueID)
		}
		ids = append(ids, fact.Operation)
	}
	if len(ids) < 2 {
		t.Fatalf("sidecar records %v; want the create and the approve label edit", ids)
	}
}
