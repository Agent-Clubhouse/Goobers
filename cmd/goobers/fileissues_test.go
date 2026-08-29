package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	fileIssuesForbiddenLbl = "goobers:approved,goobers:ready,goobers:critical,goobers:claimed,goobers:blocked-on-sibling,goobers:local,ci:flake"
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
// the run/repo/credential env the runner would inject. The write path is
// bound to a check through the checkDigest input (see writeArtifact); the
// binding test unbinds it to exercise the other arms.
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
	t.Setenv(executor.InputEnvVar("backlogLabel"), "goobers")
	t.Setenv(executor.InputEnvVar("partitionLabel"), fileIssuesTestPartLbl)
	t.Setenv(executor.InputEnvVar("maxPerRun"), "")
	t.Setenv(executor.InputEnvVar("dedupeWindowDays"), "")
	t.Setenv(executor.InputEnvVar("checkDigest"), "")
	f.resultFile = filepath.Join(t.TempDir(), fileIssuesResultFileName)
	t.Setenv(executor.InputEnvVar("resultFile"), f.resultFile)
	return f
}

// artifactEvidence is an artifact-digest evidence pointer for content: the
// strongest evidence class in the budget ordering.
func artifactEvidence(name, content string) nomination.Evidence {
	sum := sha256.Sum256([]byte(content))
	return nomination.Evidence{Kind: nomination.EvidenceArtifact, Path: "artifacts/" + name, Digest: "sha256:" + hex.EncodeToString(sum[:])}
}

// lowRisk is a well-formed low-risk nomination with artifact and source
// evidence.
func lowRisk(key string) nomination.Nomination {
	return nomination.Nomination{
		Key: key, DedupeKey: "vet:internal/worktree:" + key,
		Title:      "go vet: unused result in internal/worktree (" + key + ")",
		Body:       "go vet reports an unchecked error return in internal/worktree/manager.go; the result of Close is dropped.",
		Labels:     []string{"area:runner", "type:bug"},
		RiskClass:  nomination.RiskLow,
		RiskReason: "a vet finding with a source location and an artifact-backed stack",
		Evidence: []nomination.Evidence{
			artifactEvidence(key+"-vet.txt", "internal/worktree/manager.go:88: result of Close is not used ("+key+")"),
			{Kind: nomination.EvidenceSource, Path: "internal/worktree/manager.go", Line: 88},
		},
	}
}

// writeArtifact writes the nominations artifact into the workspace as the
// finder would (naming the stage's own run) and binds the write path to it
// through the checkDigest input — the shape a workflow wires with inputsFrom
// from the check stage's nominationsDigest output.
func (f *fileIssuesFixture) writeArtifact(nominations ...nomination.Nomination) nomination.Artifact {
	f.t.Helper()
	return f.writeArtifactForRun(fileIssuesTestRunID, nominations...)
}

func (f *fileIssuesFixture) writeArtifactForRun(runID string, nominations ...nomination.Nomination) nomination.Artifact {
	f.t.Helper()
	artifact := nomination.Artifact{
		Schema: nomination.SchemaV1, RunID: runID,
		Producer: nomination.Producer{Stage: "triage", Attempt: 1}, Nominations: nominations,
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, fileIssuesArtifactName), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	digest, err := nomination.Digest(artifact)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Setenv(executor.InputEnvVar("checkDigest"), digest)
	return artifact
}

// unbind removes the checkDigest input entirely (an absent input, not an
// empty one), so the write path falls through to the checkFile and journal
// arms.
func (f *fileIssuesFixture) unbind() {
	f.t.Helper()
	f.t.Setenv(executor.InputEnvVar("checkDigest"), "")
	if err := os.Unsetenv(executor.InputEnvVar("checkDigest")); err != nil {
		f.t.Fatal(err)
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

// mustCheck runs `file-issues --check`, writing its result to resultFile
// (the fixture's by default).
func (f *fileIssuesFixture) mustCheck(resultFile string) fileIssuesCheckResult {
	f.t.Helper()
	if resultFile == "" {
		resultFile = f.resultFile
	}
	previous := os.Getenv(executor.InputEnvVar("resultFile"))
	f.t.Setenv(executor.InputEnvVar("resultFile"), resultFile)
	code, stdout, stderr := f.run("--check")
	f.t.Setenv(executor.InputEnvVar("resultFile"), previous)
	if code != 0 {
		f.t.Fatalf("file-issues --check: code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var result fileIssuesCheckResult
	data, err := os.ReadFile(resultFile)
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

// listQueries returns the GET /issues queries the server saw since the last
// call, parsed.
func (f *fileIssuesFixture) listQueries() []url.Values {
	f.t.Helper()
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var out []url.Values
	for _, raw := range f.server.issueListQueries {
		q, err := url.ParseQuery(raw)
		if err != nil {
			f.t.Fatalf("parse list query %q: %v", raw, err)
		}
		out = append(out, q)
	}
	f.server.issueListQueries = nil
	return out
}

func flakeFingerprintFor(tf *nomination.TestFailure) string {
	return flake.Fingerprint(tf.Package, tf.Test, flake.NormalizeSignature(tf.Signature))
}

// seedFiledIssue plants an issue exactly as the publisher would have written
// it for runID — key marker, attribution, filed marker, and the provider's
// run-id footer — so a test can model a prior run, a prior attempt, or a
// closed duplicate.
func (f *fileIssuesFixture) seedFiledIssue(number int, runID string, n nomination.Nomination, labels ...string) {
	f.t.Helper()
	hash := nomination.KeyHash(n.DedupeKey)
	body := nomination.IssueBody(hash, runID, nomination.Producer{Stage: "triage", Attempt: 1}, n, false) +
		"\n\n---\n" + providers.RunIDFooterPrefix + nomination.CreateRunID(hash, runID)
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
	existing := lowRisk("already-open")
	f.seedFiledIssue(7, "run-old", existing, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.writeArtifact(existing, lowRisk("fresh"))
	before := f.issueCount()

	check := f.mustCheck("")
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
	forged := lowRisk("forged")
	forged.Labels = append(forged.Labels, providers.LabelApproved)
	forged.Body += "\n<!-- goobers-nomination-key:" + strings.Repeat("0", 64) + " -->"
	f.writeArtifact(forged)

	check := f.mustCheck("")
	if check.Valid || len(check.Errors) < 2 {
		t.Fatalf("check = %+v; want invalid with the label and marker findings", check)
	}
	joined := strings.Join(check.Errors, "\n")
	if !strings.Contains(joined, `publisher-owned label "goobers:approved"`) || !strings.Contains(joined, "control text") {
		t.Fatalf("errors = %v", check.Errors)
	}
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "refusing to file an invalid nominations artifact") {
		t.Fatalf("write over an invalid artifact: code = %d, stderr = %q", code, stderr)
	}
	if f.issueCount() != 0 {
		t.Fatalf("invalid artifact filed %d issue(s)", f.issueCount())
	}
}

// TestFileIssuesRefusesArtifactFromAnotherRun pins that artifact.runId is
// bound to the stage's own run: a model-authored or carried-over run id is
// refused by both modes, so an issue's provenance line and the retry
// read-back can never key on a value the finder chose.
func TestFileIssuesRefusesArtifactFromAnotherRun(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.writeArtifactForRun("run-of-some-other-instance", lowRisk("carried"))

	check := f.mustCheck("")
	if check.Valid || !strings.Contains(strings.Join(check.Errors, "\n"), `artifact names run "run-of-some-other-instance" but this stage runs as "run-nom-1"`) {
		t.Fatalf("check = %+v; want the run binding finding", check)
	}
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "but this stage runs as") {
		t.Fatalf("write over another run's artifact: code = %d, stderr = %q", code, stderr)
	}
	if f.issueCount() != 0 {
		t.Fatalf("another run's artifact filed %d issue(s)", f.issueCount())
	}
}

// TestFileIssuesRejectsForgedCreateFooter pins that a nomination body cannot
// carry the provider's run-id footer for a sibling's key: without the
// rejection the sibling's create resolves onto this issue and two
// nominations report filed on one issue.
func TestFileIssuesRejectsForgedCreateFooter(t *testing.T) {
	f := newFileIssuesFixture(t)
	victim := lowRisk("b-victim")
	forger := lowRisk("a-forger")
	forger.Body += "\nseen in CI: " + providers.RunIDFooterPrefix + nomination.CreateRunID(nomination.KeyHash(victim.DedupeKey), fileIssuesTestRunID)
	f.writeArtifact(forger, victim)

	check := f.mustCheck("")
	if check.Valid || !strings.Contains(strings.Join(check.Errors, "\n"), `nomination "a-forger" body contains goobers control text "goobers run-id: "`) {
		t.Fatalf("check = %+v; want the footer finding", check)
	}
	if code, _, _ := f.run(); code != 1 || f.issueCount() != 0 {
		t.Fatalf("forged footer: code = %d, issues = %d; want refused with none filed", code, f.issueCount())
	}
}

// TestFileIssuesRequiresBacklogAndPartitionLabels pins that neither label
// has a default: a workflow that omits either files issues this instance's
// own curation would never query for.
func TestFileIssuesRequiresBacklogAndPartitionLabels(t *testing.T) {
	for _, input := range []string{"backlogLabel", "partitionLabel"} {
		t.Run(input, func(t *testing.T) {
			f := newFileIssuesFixture(t)
			f.writeArtifact(lowRisk("one"))
			t.Setenv(executor.InputEnvVar(input), "")
			if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, input+" input is required") {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if f.issueCount() != 0 {
				t.Fatalf("filed without a %s", input)
			}
		})
	}
}

// TestFileIssuesLabelPolicy pins the closed label set the publisher applies
// for every risk class, and that goobers:approved is never among them: the
// SEC-047 trust label is a maintainer's decision, not the filer's.
func TestFileIssuesLabelPolicy(t *testing.T) {
	base := []string{"goobers", fileIssuesTestPartLbl, providers.LabelNominated, "area:runner", "type:bug"}
	cases := []struct {
		name        string
		risk        nomination.RiskClass
		humanReview bool
		want        []string
	}{
		{name: "low files unapproved", risk: nomination.RiskLow, want: base},
		{name: "standard files unapproved", risk: nomination.RiskStandard, want: base},
		{name: "human files needs-human", risk: nomination.RiskHuman, want: append(slices.Clone(base), providers.LabelNeedsHuman)},
		{name: "finding requiring human review files needs-human", risk: nomination.RiskLow, humanReview: true, want: append(slices.Clone(base), providers.LabelNeedsHuman)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFileIssuesFixture(t)
			// An approve credential in the env changes nothing: the stage never
			// asks for it.
			t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_APPROVE", "approve-token")
			n := lowRisk("policy")
			n.RiskClass = tc.risk
			n.RequiresHumanReview = tc.humanReview
			f.writeArtifact(n)

			result := f.mustRun()
			if result.Created != 1 || len(result.Issues) != 1 {
				t.Fatalf("result = %+v; want one created issue", result)
			}
			number := f.issueNumber(result.Issues[0].IssueID)
			got := f.server.issueLabels(number)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("labels = %v, want %v", got, want)
			}
			assertNoForbiddenLabels(t, got)
			body := f.issueBody(number)
			hash := nomination.KeyHash(n.DedupeKey)
			if _, ok := nomination.ParseKeyMarker(body); !ok || !strings.HasPrefix(body, nomination.KeyMarker(hash)) {
				t.Fatalf("body does not start with the key marker:\n%s", body)
			}
			if !strings.Contains(body, nomination.FiledMarker(hash, fileIssuesTestRunID)) || !strings.Contains(body, "Nominated by run `"+fileIssuesTestRunID+"` (stage `triage`, attempt 1).") {
				t.Fatalf("body does not attribute the filing run by marker and by line:\n%s", body)
			}
			if wantHuman := slices.Contains(want, providers.LabelNeedsHuman); wantHuman != strings.Contains(body, "For the human:") {
				t.Fatalf("For the human block present = %v, want %v:\n%s", !wantHuman, wantHuman, body)
			}
		})
	}

	t.Run("no approve flag exists", func(t *testing.T) {
		f := newFileIssuesFixture(t)
		f.writeArtifact(lowRisk("flag"))
		if code, _, _ := f.run("--auto-approve"); code != 2 {
			t.Fatalf("--auto-approve: code = %d, want usage error", code)
		}
		if f.issueCount() != 0 {
			t.Fatal("a usage error filed an issue")
		}
	})
}

func TestFileIssuesDedupeOpenAlwaysClosedWithinWindow(t *testing.T) {
	f := newFileIssuesFixture(t)
	openDup := lowRisk("open-dup")
	closedRecent := lowRisk("closed-recent")
	closedOld := lowRisk("closed-old")
	f.seedFiledIssue(10, "run-old", openDup, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.setIssueUpdatedAt(10, time.Now().Add(-400*24*time.Hour)) // age never matters for an open match
	f.seedFiledIssue(11, "run-old", closedRecent, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.closeIssue(11)
	f.server.setIssueUpdatedAt(11, time.Now().Add(-5*24*time.Hour))
	f.seedFiledIssue(12, "run-old", closedOld, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.closeIssue(12)
	f.server.setIssueUpdatedAt(12, time.Now().Add(-40*24*time.Hour))
	f.writeArtifact(openDup, closedRecent, closedOld)
	f.listQueries()

	result := f.mustRun()
	if result.Created != 1 || result.Suppressed != 2 || len(result.Issues) != 1 || result.Issues[0].Key != "closed-old" {
		t.Fatalf("result = %+v; want closed-old filed and the other two suppressed", result)
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
	// The dedupe listing is bounded: every open nominated issue, and only the
	// closed ones updated inside the window — never an unbounded state=all
	// listing of everything the repository ever accumulated.
	var nominatedStates, closedSince []string
	for _, q := range f.listQueries() {
		if !strings.Contains(q.Get("labels"), providers.LabelNominated) {
			continue
		}
		nominatedStates = append(nominatedStates, q.Get("state"))
		if q.Get("state") == "closed" {
			closedSince = append(closedSince, q.Get("since"))
		}
	}
	slices.Sort(nominatedStates)
	nominatedStates = slices.Compact(nominatedStates)
	if !slices.Equal(nominatedStates, []string{"closed", "open"}) || len(closedSince) == 0 {
		t.Fatalf("nominated listings by state = %v, closed since = %v; want open and windowed closed", nominatedStates, closedSince)
	}
	for _, raw := range closedSince {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil || time.Since(since) > 22*24*time.Hour || time.Since(since) < 20*24*time.Hour {
			t.Fatalf("closed listing since = %q (%v); want the 21-day cutoff", raw, err)
		}
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
	// only the open match and never lists closed issues at all.
	t.Setenv(executor.InputEnvVar("dedupeWindowDays"), "60")
	t.Setenv("GOOBERS_RUN_ID", "run-nom-3")
	f.writeArtifactForRun("run-nom-3", openDup, closedRecent, closedOld)
	f.server.closeIssue(f.issueNumber(result.Issues[0].IssueID))
	f.server.setIssueUpdatedAt(f.issueNumber(result.Issues[0].IssueID), time.Now().Add(-50*24*time.Hour))
	if wide := f.mustRun(); wide.Created != 0 || wide.Suppressed != 3 {
		t.Fatalf("60-day window: %+v", wide)
	}
	t.Setenv(executor.InputEnvVar("dedupeWindowDays"), "0")
	t.Setenv("GOOBERS_RUN_ID", "run-nom-4")
	f.writeArtifactForRun("run-nom-4", openDup, closedRecent, closedOld)
	f.listQueries()
	if none := f.mustRun(); none.Created != 2 || none.Suppressed != 1 {
		t.Fatalf("zero-day window: %+v", none)
	}
	for _, q := range f.listQueries() {
		if strings.Contains(q.Get("labels"), providers.LabelNominated) && q.Get("state") != "open" {
			t.Fatalf("zero-day window listed nominated issues with state=%q", q.Get("state"))
		}
	}
}

// TestFileIssuesPrefersTheEarliestPriorIssueNumerically pins that "the
// earliest prior issue" is #9 before #10: the ordering picks which open
// duplicate a suppression names and annotates.
func TestFileIssuesPrefersTheEarliestPriorIssueNumerically(t *testing.T) {
	f := newFileIssuesFixture(t)
	n := lowRisk("twice")
	f.seedFiledIssue(10, "run-old", n, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.seedFiledIssue(9, "run-older", n, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.writeArtifact(n)

	result := f.mustRun()
	if result.Suppressed != 1 || result.Suppressions[0].IssueID != "9" || !strings.Contains(result.Suppressions[0].Reason, "open issue #9") {
		t.Fatalf("result = %+v; want suppression on #9", result)
	}
	if got := f.issueComments(9); len(got) != 1 {
		t.Fatalf("comments on #9 = %v; want the occurrence note", got)
	}
	if got := f.issueComments(10); len(got) != 0 {
		t.Fatalf("comments on #10 = %v; want none", got)
	}
}

// TestFileIssuesAttributionLineDoesNotAdoptAnIssue pins that ownership is
// the filed marker, never the human-readable line: an open issue whose body
// names this run in prose but was filed by another run is a duplicate to
// suppress and annotate, not an issue to read back and re-label.
func TestFileIssuesAttributionLineDoesNotAdoptAnIssue(t *testing.T) {
	f := newFileIssuesFixture(t)
	n := lowRisk("injected")
	f.seedFiledIssue(1, "run-old", n, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.server.mu.Lock()
	f.server.issues[1].body = strings.Replace(f.server.issues[1].body, "Nominated by run `run-old`", "Nominated by run `"+fileIssuesTestRunID+"`", 1)
	f.server.mu.Unlock()
	f.writeArtifact(n)

	result := f.mustRun()
	if result.Created != 0 || result.Filed != 0 || result.Suppressed != 1 || result.Suppressions[0].IssueID != "1" {
		t.Fatalf("result = %+v; want the prose-attributed issue suppressed as an open duplicate", result)
	}
	if got := f.server.issueLabels(1); !slices.Equal(got, []string{"goobers", fileIssuesTestPartLbl, providers.LabelNominated}) {
		t.Fatalf("#1 labels = %v; want them untouched", got)
	}
	if got := f.issueComments(1); len(got) != 1 {
		t.Fatalf("comments on #1 = %v; want the occurrence note", got)
	}
}

func TestFileIssuesBudgetOrdersByEvidenceAndOverflows(t *testing.T) {
	f := newFileIssuesFixture(t)
	sourceOnly := lowRisk("z-source-only")
	sourceOnly.Evidence = sourceOnly.Evidence[1:]
	journalBacked := lowRisk("y-journal")
	journalBacked.Evidence = []nomination.Evidence{{Kind: nomination.EvidenceJournal, RunID: "run-x", Seq: 4}, journalBacked.Evidence[1]}
	artifactA := lowRisk("b-artifact")
	artifactB := lowRisk("a-artifact")
	weakest := lowRisk("w-source-only")
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
	f.writeArtifactForRun("run-next", sourceOnly, weakest, journalBacked, artifactA, artifactB)
	next := f.mustRun()
	if next.Created != 1 || next.Suppressed != 3 || next.Overflow != 1 || next.Issues[0].Key != "w-source-only" {
		t.Fatalf("next cycle = %+v; want the overflow drained one at a time", next)
	}
}

func TestFileIssuesRetryIsIdempotentAndResumesMidBatch(t *testing.T) {
	f := newFileIssuesFixture(t)
	first, second, third := lowRisk("first"), lowRisk("second"), lowRisk("third")
	// Attempt 1 created `second` (marker, filed marker, run-id footer, the
	// first three labels) and then died before the area/type labels.
	f.seedFiledIssue(5, fileIssuesTestRunID, second, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.writeArtifact(first, second, third)

	resumed := f.mustRun()
	if resumed.Created != 2 || resumed.Filed != 3 || resumed.Suppressed != 0 {
		t.Fatalf("resumed attempt = %+v; want only the missing remainder created", resumed)
	}
	for _, issue := range resumed.Issues {
		if issue.Key == "second" && (issue.IssueID != "5" || !issue.Reused) {
			t.Fatalf("second was not resumed onto #5: %+v", issue)
		}
	}
	if got := f.server.issueLabels(5); !slices.Contains(got, "area:runner") || !slices.Contains(got, "type:bug") {
		t.Fatalf("#5 labels = %v; want the label set made whole", got)
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
	if retried.Created != 0 || retried.Filed != 3 || f.issueCount() != 3 {
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
	f.writeArtifactForRun("run-tomorrow", first, second, third)
	later := f.mustRun()
	if later.Created != 0 || later.Suppressed != 3 || f.issueCount() != 3 {
		t.Fatalf("next-day run = %+v, issues = %d", later, f.issueCount())
	}
}

func TestFileIssuesNeverLabelsFlakeWatchIssues(t *testing.T) {
	f := newFileIssuesFixture(t)

	// A flake-watch issue whose fingerprint matches the nomination's test
	// failure suppresses it outright.
	fingerprinted := lowRisk("flaky-test")
	fingerprinted.TestFailure = &nomination.TestFailure{
		Package: "github.com/goobers/goobers/internal/runner", Test: "TestRunnerRace",
		Signature: "run_test.go:41: expected 3 got 2\n    goroutine 17 [running]:",
	}
	f.server.addIssue(20, "[flake] TestRunnerRace", nomination.FlakeLabel)
	f.server.mu.Lock()
	f.server.issues[20].body = "<!-- goobers-flake-fingerprint:" + flakeFingerprintFor(fingerprinted.TestFailure) + " -->\nowned by flake-watch"
	f.server.mu.Unlock()

	// An issue that carries ci:flake AND the nomination's run-id footer (the
	// provider's create lookup finds it) must not receive a single goobers
	// label.
	adopted := lowRisk("adopted-by-flake-watch")
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

// TestFileIssuesBindsTheWriteToACheck pins the fail-closed binding and its
// three arms: the checkDigest input (a pod's only route), the check's result
// file on disk, and the check stage's result recorded in the run journal (a
// self runner). No hand-copying between the check and the write.
func TestFileIssuesBindsTheWriteToACheck(t *testing.T) {
	f := newFileIssuesFixture(t)
	artifact := f.writeArtifact(lowRisk("bound"))
	digest, err := nomination.Digest(artifact)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no check anywhere fails closed", func(t *testing.T) {
		f.unbind()
		code, _, stderr := f.run()
		if code != 1 || !strings.Contains(stderr, "no file-issues --check result to bind to") || !strings.Contains(stderr, "checkDigest") {
			t.Fatalf("code = %d, stderr = %q", code, stderr)
		}
		if f.issueCount() != 0 {
			t.Fatal("filed without a check")
		}
	})
	t.Run("checkDigest input", func(t *testing.T) {
		t.Setenv(executor.InputEnvVar("checkDigest"), "")
		if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "checkDigest input is empty") {
			t.Fatalf("empty digest: code = %d, stderr = %q", code, stderr)
		}
		t.Setenv(executor.InputEnvVar("checkDigest"), "sha256:"+strings.Repeat("0", 64))
		if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "do not match the artifact file-issues --check marked valid (checkDigest input)") {
			t.Fatalf("stale digest: code = %d, stderr = %q", code, stderr)
		}
		if f.issueCount() != 0 {
			t.Fatal("filed on an unmatched checkDigest")
		}
		t.Setenv(executor.InputEnvVar("checkDigest"), digest)
		if result := f.mustRun(); result.Created != 1 {
			t.Fatalf("bound write = %+v", result)
		}
	})
	t.Run("check result file", func(t *testing.T) {
		f.unbind()
		t.Setenv("GOOBERS_RUN_ID", "run-nom-2")
		f.writeArtifactForRun("run-nom-2", lowRisk("bound-by-file"))
		f.unbind()
		checkFile := filepath.Join(f.workspace, fileIssuesCheckFileName)
		if check := f.mustCheck(checkFile); !check.Valid {
			t.Fatalf("check = %+v", check)
		}
		if result := f.mustRun(); result.Created != 1 {
			t.Fatalf("write bound by the check file = %+v", result)
		}
		// The check is over the exact artifact: a changed artifact must be
		// re-checked, and an invalid check binds nothing.
		f.writeArtifactForRun("run-nom-2", lowRisk("changed-since-check"))
		f.unbind()
		if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "do not match the artifact file-issues --check marked valid") {
			t.Fatalf("digest mismatch: code = %d, stderr = %q", code, stderr)
		}
		if err := os.WriteFile(checkFile, []byte(`{"valid":false,"errors":["nomination 0 has an empty title"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "did not mark valid") {
			t.Fatalf("invalid check: code = %d, stderr = %q", code, stderr)
		}
		if err := os.Remove(checkFile); err != nil {
			t.Fatal(err)
		}
		if f.issueCount() != 2 {
			t.Fatalf("issue count = %d, want 2", f.issueCount())
		}
	})
	t.Run("check stage result in the run journal", func(t *testing.T) {
		const runID = "run-nom-3"
		t.Setenv("GOOBERS_RUN_ID", runID)
		artifact := f.writeArtifactForRun(runID, lowRisk("bound-by-journal"))
		f.unbind()
		digest, err := nomination.Digest(artifact)
		if err != nil {
			t.Fatal(err)
		}
		run, err := journal.Create(layoutFor(f.root).RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "defect-nomination", Gaggle: "goobers"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := json.Marshal(fileIssuesCheckResult{Valid: true, NominationsDigest: digest, Errors: []string{}})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := run.RecordArtifact(runID+":validate-nominations/result", recorded)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "validate-nominations", Attempt: 1, Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{ref}}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if result := f.mustRun(); result.Created != 1 {
			t.Fatalf("write bound by the journal = %+v", result)
		}
	})
}

func TestFileIssuesRecordsEveryMutationInTheSidecar(t *testing.T) {
	f := newFileIssuesFixture(t)
	resumed := lowRisk("resumed")
	f.seedFiledIssue(5, fileIssuesTestRunID, resumed, "goobers", fileIssuesTestPartLbl, providers.LabelNominated)
	f.writeArtifact(lowRisk("created"), resumed)
	result := f.mustRun()
	if result.Created != 1 || result.Filed != 2 {
		t.Fatalf("result = %+v; want one created and one resumed", result)
	}
	data, err := os.ReadFile(filepath.Join(f.workspace, mutationsSidecarFile))
	if err != nil {
		t.Fatalf("read mutation sidecar: %v", err)
	}
	touched := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var fact mutationFact
		if err := json.Unmarshal([]byte(line), &fact); err != nil {
			t.Fatalf("parse sidecar line %q: %v", line, err)
		}
		if fact.Kind != "issue" {
			t.Fatalf("sidecar fact %+v is not an issue mutation", fact)
		}
		touched[fact.ID] = true
	}
	for _, issue := range result.Issues {
		if !touched[issue.IssueID] {
			t.Fatalf("sidecar %v does not name issue #%s (%s)", touched, issue.IssueID, issue.Key)
		}
	}
}

// TestFileIssuesRefusesNonGitHubProviders is the stage's provider-dispatch
// coverage: file-issues is GitHub-only in this slice and says so with a typed
// refusal before any provider is constructed, on both ADO and Gitea.
func TestFileIssuesRefusesNonGitHubProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{providers.ProviderADO, providers.ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFileIssuesFixture(t)
			f.writeArtifact(lowRisk("elsewhere"))
			setNonGitHubStageEnv(t, kind)
			previous := newADOProviderForStage
			newADOProviderForStage = func(_ string, repo providers.RepositoryRef) (*providers.ADOProvider, error) {
				t.Fatalf("file-issues constructed an ADO provider for %+v", repo)
				return nil, nil
			}
			t.Cleanup(func() { newADOProviderForStage = previous })

			for _, args := range [][]string{nil, {"--check"}} {
				code, _, stderr := f.run(args...)
				if code != 1 || !strings.Contains(stderr, `file-issues does not support repository provider "`+string(kind)+`"`) {
					t.Fatalf("file-issues %v on %s: code = %d, stderr = %q", args, kind, code, stderr)
				}
			}
			if f.issueCount() != 0 {
				t.Fatal("a refused provider filed an issue")
			}
		})
	}
}
