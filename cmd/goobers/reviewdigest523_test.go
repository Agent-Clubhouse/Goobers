package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// #523's CLI-level tests for the verdict-level digest cache (the maintainer
// ruling's core deliverable): gather-sibling-context computes reviewDigest
// and checks the selected PR's own most recent verdict comment for a
// digest match, emitting cachedVerdictJson only on a genuine match — the
// signal internal/runner's evaluateGate uses to skip the reviewer's LLM
// call entirely (proven at the gate.Evaluator layer by
// TestEvaluatorReusesCachedVerdictWithoutReviewerCall in internal/gate).

// TestComputeReviewDigestIsSensitiveToEveryPinnedInput is the digest
// formula's own contract (re-keyed by issue #718 to actually hit on a
// clean rebase): identical inputs produce identical digests regardless of
// sibling order (never semantically meaningful); a genuine patch-content
// change, a base movement that now intersects the PR's files, and the
// sibling SET changing all change the digest.
func TestComputeReviewDigestIsSensitiveToEveryPinnedInput(t *testing.T) {
	siblings := []siblingPR{
		{Number: 11, Files: []string{"a.go"}},
		{Number: 12, Files: []string{"b.go"}},
	}
	reordered := []siblingPR{
		{Number: 12, Files: []string{"b.go"}},
		{Number: 11, Files: []string{"a.go"}},
	}
	base := computeReviewDigest("patch10", "base-intersection-empty", siblings)

	if got := computeReviewDigest("patch10", "base-intersection-empty", reordered); got != base {
		t.Fatalf("digest changed with sibling order = %q, want stable digest %q (siblings must be sorted before hashing)", got, base)
	}
	if got := computeReviewDigest("patch10-changed", "base-intersection-empty", siblings); got == base {
		t.Fatalf("digest unchanged after the selected PR's own patch content changed, want it to differ from %q", base)
	}
	if got := computeReviewDigest("patch10", "base-intersection-nonempty", siblings); got == base {
		t.Fatalf("digest unchanged after base's movement started intersecting the PR's files, want it to differ from %q", base)
	}
	changedSiblingFiles := []siblingPR{{Number: 11, Files: []string{"a.go", "c.go"}}, {Number: 12, Files: []string{"b.go"}}}
	if got := computeReviewDigest("patch10", "base-intersection-empty", changedSiblingFiles); got == base {
		t.Fatalf("digest unchanged after a sibling's file set changed, want it to differ from %q", base)
	}
	fewerSiblings := []siblingPR{{Number: 11, Files: []string{"a.go"}}}
	if got := computeReviewDigest("patch10", "base-intersection-empty", fewerSiblings); got == base {
		t.Fatalf("digest unchanged after the sibling set shrank, want it to differ from %q", base)
	}
}

// TestComputeReviewDigestIgnoresSiblingHeadSHAAlone is #718's headline
// property for siblings: a force-push that doesn't change WHICH files a
// sibling touches must NOT perturb the digest — the digest formula doesn't
// even look at HeadSHA anymore, so this is really a "the field is
// ignored" proof, guarding against a future edit accidentally
// reintroducing it.
func TestComputeReviewDigestIgnoresSiblingHeadSHAAlone(t *testing.T) {
	before := []siblingPR{{Number: 11, HeadSHA: "sha-before", Files: []string{"a.go"}}}
	after := []siblingPR{{Number: 11, HeadSHA: "sha-after-forcepush", Files: []string{"a.go"}}}
	if got, want := computeReviewDigest("patch10", "base-empty", after), computeReviewDigest("patch10", "base-empty", before); got != want {
		t.Fatalf("digest changed = %q, want unchanged %q — a sibling force-push that doesn't change its file set must not invalidate the cache", got, want)
	}
}

// TestPatchIdentityIgnoresHunkLineNumberShift is #718's headline property
// for the selected PR: a rebase-shifted hunk header (line numbers moved by
// an earlier, unrelated change in the same file) must not perturb the
// identity — only the actual +/- content does.
func TestPatchIdentityIgnoresHunkLineNumberShift(t *testing.T) {
	unshifted := []providers.ChangedFile{{Path: "foo.go", Status: "modified", Patch: "@@ -10,3 +10,3 @@\n line a\n-old\n+new\n"}}
	shifted := []providers.ChangedFile{{Path: "foo.go", Status: "modified", Patch: "@@ -50,3 +50,3 @@\n line a\n-old\n+new\n"}}
	if got, want := patchIdentity(shifted), patchIdentity(unshifted); got != want {
		t.Fatalf("patchIdentity changed = %q, want unchanged %q — a hunk line-number shift alone must not change the identity", got, want)
	}
	realChange := []providers.ChangedFile{{Path: "foo.go", Status: "modified", Patch: "@@ -10,3 +10,3 @@\n line a\n-old\n+something else entirely\n"}}
	if got, unwanted := patchIdentity(realChange), patchIdentity(unshifted); got == unwanted {
		t.Fatalf("patchIdentity unchanged = %q, want it to differ — the actual patch content changed", got)
	}
}

// selectedPRFiles is the fixture's selected PR #10's own changed files —
// shared between seedVerdictCacheFixture's addCompare registration and its
// wantDigest computation so the two can never silently drift apart.
var selectedPRFiles = []fakePRFile{{path: "cmd/goobers/foo.go", status: "modified", additions: 3, deletions: 1, patch: "@@ -1,3 +1,3 @@\n a\n-b\n+c\n"}}

// seedVerdictCacheFixture stands up the shared fixture: selected PR #10
// (with a matching issue record, since verdict comments post to the issues
// API) plus one sibling #11. Registers the compare(base...head) fixture
// #718's patch-identity computation needs — base hasn't moved past the
// PR's own merge-base in this fixture (mergeBaseSHA == baseSHA), so no
// second compare (for base's own movement) is needed.
func seedVerdictCacheFixture(t *testing.T) (root string, server *fakeGitHubServer, wantDigest string) {
	t.Helper()
	root = initDemo(t)
	server = newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10head", "shamainbase", false, nil, selectedPRFiles)
	server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11head", "shamainbase",
		false, nil, []fakePRFile{{path: "internal/runner/run.go", status: "modified", additions: 1, deletions: 0}})
	server.addCompare("shamainbase", "sha10head", "shamainbase", selectedPRFiles)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-2")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")

	patchID := patchIdentity(toChangedFiles(selectedPRFiles))
	wantDigest = computeReviewDigest(patchID, sortedFileSetDigest(nil), []siblingPR{{Number: 11, Files: []string{"internal/runner/run.go"}}})
	return root, server, wantDigest
}

// toChangedFiles converts fixture files to providers.ChangedFile, mirroring
// exactly what GitHubProvider.CompareCommits/PullRequestFiles produce from
// the same shape — kept in lockstep with providercmd_test.go's
// handleCompare/handlePullItem so a test's expected digest is always
// computed from what the fake server will actually report.
func toChangedFiles(files []fakePRFile) []providers.ChangedFile {
	out := make([]providers.ChangedFile, 0, len(files))
	for _, f := range files {
		out = append(out, providers.ChangedFile{Path: f.path, Status: f.status, Additions: f.additions, Deletions: f.deletions, Patch: f.patch})
	}
	return out
}

// TestGatherSiblingContextFindsMatchingCachedVerdict is #523's headline
// verdict-cache acceptance: a prior run's posted verdict comment whose
// Digest matches this gather's freshly computed reviewDigest is surfaced as
// cachedVerdictJson, reused verbatim.
func TestGatherSiblingContextFindsMatchingCachedVerdict(t *testing.T) {
	root, server, wantDigest := seedVerdictCacheFixture(t)
	prior := apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "looks good", Digest: wantDigest,
		SourceRunID: "run-1", HeadSHA: "sha10head", BaseSHA: "shamainbase",
	}
	comment := renderVerdictComment(prior)
	server.addComment(10, comment)

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "gather-sibling-context", root)
	if code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var full struct {
		Siblings []siblingPR `json:"siblings"`
		siblingContextResult
	}
	if err := json.Unmarshal(data, &full); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	if len(full.Siblings) != 1 || full.Siblings[0].Number != 11 {
		t.Fatalf("siblings = %+v, want exactly #11", full.Siblings)
	}

	resultData := full.siblingContextResult
	if resultData.ReviewDigest != wantDigest {
		t.Fatalf("reviewDigest = %q, want %q", resultData.ReviewDigest, wantDigest)
	}
	if resultData.CachedVerdictJSON == "" {
		t.Fatalf("cachedVerdictJson is empty, want the matching prior verdict")
	}
	var got apiv1.Verdict
	if err := json.Unmarshal([]byte(resultData.CachedVerdictJSON), &got); err != nil {
		t.Fatalf("unmarshal cachedVerdictJson: %v", err)
	}
	if got.Decision != prior.Decision || got.Digest != prior.Digest || got.SourceRunID != prior.SourceRunID {
		t.Fatalf("cached verdict = %+v, want %+v reused verbatim", got, prior)
	}
	if !strings.Contains(stdout, "verdict cache HIT") {
		t.Fatalf("stdout = %q, want it to report a verdict cache hit", stdout)
	}
}

// TestGatherSiblingContextCleanRebaseHitsCache is issue #718's headline
// acceptance criterion: a PR reviewed once, then cleanly rebased (its head
// SHA changes — a real force-push — but its actual patch content does
// not), still hits the verdict cache on the next gather. Before #718 this
// was the dominant 0/16-hit scenario (computeReviewDigest hashed the raw
// head SHA, so ANY rebase — clean or not — was always a miss).
func TestGatherSiblingContextCleanRebaseHitsCache(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(30, "Selected PR")
	preRebaseFiles := []fakePRFile{{path: "cmd/goobers/foo.go", status: "modified", additions: 3, deletions: 1, patch: "@@ -10,3 +10,3 @@\n a\n-b\n+c\n"}}
	server.addOpenPR(30, "goobers/implementation/run-30", "main", "sha30-before-rebase", "shamainbase", false, nil, preRebaseFiles)
	server.addCompare("shamainbase", "sha30-before-rebase", "shamainbase", preRebaseFiles)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "30")

	// A real reviewer produced a verdict for the PR's pre-rebase state.
	preRebaseDigest := computeReviewDigest(patchIdentity(toChangedFiles(preRebaseFiles)), sortedFileSetDigest(nil), nil)
	prior := apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "looks good", Digest: preRebaseDigest,
		SourceRunID: "run-review", HeadSHA: "sha30-before-rebase", BaseSHA: "shamainbase",
	}
	server.addComment(30, renderVerdictComment(prior))

	// The PR gets cleanly rebased: a real force-push changes its head SHA,
	// but git patch-id (and this stage's patchIdentity, its content-hash
	// alternative) would report the SAME patch — same path, status, and
	// hunk content, only the SHA differs. Base does not move in this
	// scenario (a clean rebase onto the SAME base tip — e.g. an amend/
	// force-push with no base change at all — still must not be confused
	// with a stale review; base movement is covered separately by
	// TestGatherSiblingContextBaseMovementDisjointFromPRStillHitsCache).
	server.setPRHead(30, "sha30-after-rebase", preRebaseFiles)
	server.addCompare("shamainbase", "sha30-after-rebase", "shamainbase", preRebaseFiles)

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "gather-sibling-context", root)
	if code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var result siblingContextResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	if result.CachedVerdictJSON == "" {
		t.Fatalf("cachedVerdictJson is empty — the clean rebase should have hit the verdict cache (stdout=%q)", stdout)
	}
	var got apiv1.Verdict
	if err := json.Unmarshal([]byte(result.CachedVerdictJSON), &got); err != nil {
		t.Fatalf("unmarshal cachedVerdictJson: %v", err)
	}
	if got.Digest != preRebaseDigest {
		t.Fatalf("cached verdict digest = %q, want the pre-rebase verdict's own digest %q reused verbatim", got.Digest, preRebaseDigest)
	}
	if !strings.Contains(stdout, "verdict cache HIT") {
		t.Fatalf("stdout = %q, want it to report a verdict cache hit", stdout)
	}
}

// TestGatherSiblingContextBaseMovementDisjointFromPRStillHitsCache is issue
// #718's other headline acceptance criterion, at the gather-sibling-context
// integration level (TestComputeReviewDigestIsSensitiveToEveryPinnedInput
// already proves it at the pure-digest unit level): base advancing past
// this PR's own merge-base does NOT invalidate a cached verdict when the
// commits that landed on base don't touch any file this PR also changes —
// the dominant false-invalidation case before #718 (any OTHER PR merging
// advanced base for every open PR).
func TestGatherSiblingContextBaseMovementDisjointFromPRStillHitsCache(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(40, "Selected PR")
	ownFiles := []fakePRFile{{path: "cmd/goobers/foo.go", status: "modified", patch: "@@ -1,1 +1,1 @@\n-a\n+b\n"}}
	server.addOpenPR(40, "goobers/implementation/run-40", "main", "sha40head", "shamainbase-v1", false, nil, ownFiles)
	server.addCompare("shamainbase-v1", "sha40head", "shamainbase-v1", ownFiles)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "40")

	reviewedDigest := computeReviewDigest(patchIdentity(toChangedFiles(ownFiles)), sortedFileSetDigest(nil), nil)
	prior := apiv1.Verdict{
		Decision: apiv1.VerdictPass, Digest: reviewedDigest, SourceRunID: "run-review",
		HeadSHA: "sha40head", BaseSHA: "shamainbase-v1",
	}
	server.addComment(40, renderVerdictComment(prior))

	// Base advances via a disjoint, unrelated merge — the merge-base of
	// this PR's branch and base's NEW tip is unchanged (v1), but base's
	// own live SHA is now v2.
	unrelatedBaseMove := []fakePRFile{{path: "unrelated/other.go", status: "modified"}}
	server.setPRBase(40, "shamainbase-v2")
	server.addCompare("shamainbase-v2", "sha40head", "shamainbase-v1", ownFiles)
	server.addCompare("shamainbase-v1", "shamainbase-v2", "shamainbase-v1", unrelatedBaseMove)

	result := readSiblingContextResultAfterGather(t, root)
	if result.CachedVerdictJSON == "" {
		t.Fatalf("cachedVerdictJson is empty — base moved disjointly from this PR's own files and must not have invalidated the cache")
	}
}

// TestGatherSiblingContextBaseMovementIntersectingPRMissesCache is the
// converse of the disjoint case above: base's movement DOES touch a file
// this PR also changes, so the cached verdict is (correctly) no longer
// reusable — this PR's own files were never reviewed against that changed
// content.
func TestGatherSiblingContextBaseMovementIntersectingPRMissesCache(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(41, "Selected PR")
	sharedPath := "shared/conflict.go"
	ownFiles := []fakePRFile{{path: sharedPath, status: "modified", patch: "@@ -1,1 +1,1 @@\n-a\n+b\n"}}
	server.addOpenPR(41, "goobers/implementation/run-41", "main", "sha41head", "shamainbase-v1", false, nil, ownFiles)
	server.addCompare("shamainbase-v1", "sha41head", "shamainbase-v1", ownFiles)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-review")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "41")

	reviewedDigest := computeReviewDigest(patchIdentity(toChangedFiles(ownFiles)), sortedFileSetDigest(nil), nil)
	prior := apiv1.Verdict{
		Decision: apiv1.VerdictPass, Digest: reviewedDigest, SourceRunID: "run-review",
		HeadSHA: "sha41head", BaseSHA: "shamainbase-v1",
	}
	server.addComment(41, renderVerdictComment(prior))

	// Base advances via a merge that ALSO touches shared/conflict.go — the
	// exact file this PR itself changes.
	intersectingBaseMove := []fakePRFile{{path: sharedPath, status: "modified"}}
	server.setPRBase(41, "shamainbase-v2")
	server.addCompare("shamainbase-v2", "sha41head", "shamainbase-v1", ownFiles)
	server.addCompare("shamainbase-v1", "shamainbase-v2", "shamainbase-v1", intersectingBaseMove)

	result := readSiblingContextResultAfterGather(t, root)
	if result.CachedVerdictJSON != "" {
		t.Fatalf("cachedVerdictJson = %q, want empty — base's movement touched this PR's own file, the verdict must not be reused", result.CachedVerdictJSON)
	}
}

// TestGatherSiblingContextIgnoresStaleCachedVerdict: a posted verdict whose
// Digest no longer matches (e.g. it was computed before a sibling's most
// recent push) is never surfaced — the runner must fall through to a real
// review, not silently reuse stale evidence.
func TestGatherSiblingContextIgnoresStaleCachedVerdict(t *testing.T) {
	root, server, _ := seedVerdictCacheFixture(t)
	stale := apiv1.Verdict{Decision: apiv1.VerdictPass, Digest: "sha256:stale-does-not-match", SourceRunID: "run-1"}
	server.addComment(10, renderVerdictComment(stale))

	result := readSiblingContextResultAfterGather(t, root)
	if result.CachedVerdictJSON != "" {
		t.Fatalf("cachedVerdictJson = %q, want empty (stale digest must not be reused)", result.CachedVerdictJSON)
	}
}

// TestGatherSiblingContextNoVerdictCommentIsNotAnError: the common steady
// state before any merge-review run has ever posted a verdict — no cache
// hit, no error.
func TestGatherSiblingContextNoVerdictCommentIsNotAnError(t *testing.T) {
	root, _, _ := seedVerdictCacheFixture(t)
	result := readSiblingContextResultAfterGather(t, root)
	if result.CachedVerdictJSON != "" {
		t.Fatalf("cachedVerdictJson = %q, want empty (no prior comment exists)", result.CachedVerdictJSON)
	}
	if result.ReviewDigest == "" {
		t.Fatalf("reviewDigest is empty, want it always computed regardless of cache hit")
	}
}

// TestGatherSiblingContextNoVerdictCacheFlagSkipsLookup: --no-verdict-cache
// bypasses the lookup entirely, even when a matching comment exists — the
// escape hatch the ruling specifies for debug/remediation flows.
func TestGatherSiblingContextNoVerdictCacheFlagSkipsLookup(t *testing.T) {
	root, server, wantDigest := seedVerdictCacheFixture(t)
	prior := apiv1.Verdict{Decision: apiv1.VerdictPass, Digest: wantDigest, SourceRunID: "run-1"}
	server.addComment(10, renderVerdictComment(prior))

	dir := t.TempDir()
	t.Chdir(dir)
	code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root)
	if code != 0 {
		t.Fatalf("gather-sibling-context --no-verdict-cache: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var result struct {
		CachedVerdictJSON string `json:"cachedVerdictJson"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.CachedVerdictJSON != "" {
		t.Fatalf("cachedVerdictJson = %q, want empty with --no-verdict-cache set despite a matching comment existing", result.CachedVerdictJSON)
	}
}

// siblingContextResult is sibling-context.json's shape for the verdict-cache
// fields this test file asserts on.
type siblingContextResult struct {
	ReviewDigest      string `json:"reviewDigest"`
	CachedVerdictJSON string `json:"cachedVerdictJson"`
}

func readSiblingContextResult(t *testing.T, root string) siblingContextResult {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if code, _, stderr := runArgs(t, "gather-sibling-context", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var result siblingContextResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	return result
}

func readSiblingContextResultAfterGather(t *testing.T, root string) siblingContextResult {
	t.Helper()
	return readSiblingContextResult(t, root)
}

// TestApplyVerdictStampsDigestAndSourceRunIDOnFreshVerdict: a genuinely
// fresh reviewer verdict (no Digest/SourceRunID set — the reviewer goober
// has no way to know either) is stamped with this run's reviewDigest input
// and GOOBERS_RUN_ID before it's posted, so the NEXT gather-sibling-context
// can find and reuse it.
func TestApplyVerdictStampsDigestAndSourceRunIDOnFreshVerdict(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(20, "Selected PR")
	server.addOpenPR(20, "goobers/implementation/run-20", "main", "sha20head", "shamainbase", false, nil, nil)

	const runID = "run-fresh"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "20")
	t.Setenv("GOOBERS_INPUT_REVIEWDIGEST", "sha256:freshly-computed")

	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "clean", HeadSHA: "sha20head", BaseSHA: "shamainbase",
	})

	applyDir := t.TempDir()
	t.Chdir(applyDir)
	if code, _, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stderr = %q", code, stderr)
	}

	server.mu.Lock()
	issue := server.issues[20]
	server.mu.Unlock()
	if len(issue.comments) != 1 {
		t.Fatalf("comments = %v, want exactly 1", issue.comments)
	}
	posted, ok := parseVerdictComment(issue.comments[0])
	if !ok {
		t.Fatalf("posted comment has no recoverable verdict payload: %q", issue.comments[0])
	}
	if posted.Digest != "sha256:freshly-computed" {
		t.Fatalf("posted.Digest = %q, want the reviewDigest input stamped in", posted.Digest)
	}
	if posted.SourceRunID != runID {
		t.Fatalf("posted.SourceRunID = %q, want this run's own id %q stamped in", posted.SourceRunID, runID)
	}
	cached, err := findCachedVerdict(
		context.Background(),
		server.newGitHubProvider("test-token"),
		providers.RepositoryRef{Owner: "your-org", Name: "your-repo"},
		20,
		"sha256:freshly-computed",
	)
	if err != nil {
		t.Fatalf("find cached pass verdict: %v", err)
	}
	if cached == nil || cached.SourceRunID != runID {
		t.Fatalf("cached verdict = %+v, want pass verdict posted by %s", cached, runID)
	}
}

// TestApplyVerdictPreservesCacheHitVerdictDigestAndSourceRunID: a verdict
// that ALREADY carries Digest/SourceRunID (the shape gate.Evaluator
// re-journals on a cache hit — reused from whichever run originally
// produced it) must be posted unchanged, never overwritten with this run's
// own reviewDigest/run id.
func TestApplyVerdictPreservesCacheHitVerdictDigestAndSourceRunID(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(21, "Selected PR")
	server.addOpenPR(21, "goobers/implementation/run-21", "main", "sha21head", "shamainbase", false, nil, nil)

	const runID = "run-cachehit"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "21")
	t.Setenv("GOOBERS_INPUT_REVIEWDIGEST", "sha256:this-runs-own-digest")

	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "reused", HeadSHA: "sha21head", BaseSHA: "shamainbase",
		Digest: "sha256:original-producer-digest", SourceRunID: "run-original-producer",
	})

	applyDir := t.TempDir()
	t.Chdir(applyDir)
	if code, _, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stderr = %q", code, stderr)
	}

	server.mu.Lock()
	issue := server.issues[21]
	server.mu.Unlock()
	posted, ok := parseVerdictComment(issue.comments[0])
	if !ok {
		t.Fatalf("posted comment has no recoverable verdict payload: %q", issue.comments[0])
	}
	if posted.Digest != "sha256:original-producer-digest" {
		t.Fatalf("posted.Digest = %q, want the original producer's digest preserved, not this run's own", posted.Digest)
	}
	if posted.SourceRunID != "run-original-producer" {
		t.Fatalf("posted.SourceRunID = %q, want the original producer's run id preserved, not %q", posted.SourceRunID, runID)
	}
}
