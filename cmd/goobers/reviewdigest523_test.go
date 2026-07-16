package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// #523's CLI-level tests for the verdict-level digest cache (the maintainer
// ruling's core deliverable): gather-sibling-context computes reviewDigest
// and checks the selected PR's own most recent verdict comment for a
// digest match, emitting cachedVerdictJson only on a genuine match — the
// signal internal/runner's evaluateGate uses to skip the reviewer's LLM
// call entirely (proven at the gate.Evaluator layer by
// TestEvaluatorReusesCachedVerdictWithoutReviewerCall in internal/gate).

// TestComputeReviewDigestIsSensitiveToEveryPinnedInput is the digest
// formula's own contract: identical inputs produce identical digests
// regardless of sibling order (never semantically meaningful), and each of
// the ruling's four invalidation triggers — selected-PR push (head SHA),
// base advance (base SHA), a sibling's head SHA changing, and the sibling
// SET changing — changes the digest.
func TestComputeReviewDigestIsSensitiveToEveryPinnedInput(t *testing.T) {
	siblings := []siblingPR{
		{Number: 11, HeadSHA: "sha11"},
		{Number: 12, HeadSHA: "sha12"},
	}
	reordered := []siblingPR{
		{Number: 12, HeadSHA: "sha12"},
		{Number: 11, HeadSHA: "sha11"},
	}
	base := computeReviewDigest("head10", "shamainbase", siblings)

	if got := computeReviewDigest("head10", "shamainbase", reordered); got != base {
		t.Fatalf("digest changed with sibling order = %q, want stable digest %q (siblings must be sorted before hashing)", got, base)
	}
	if got := computeReviewDigest("head10-pushed", "shamainbase", siblings); got == base {
		t.Fatalf("digest unchanged after selected-PR push, want it to differ from %q", base)
	}
	if got := computeReviewDigest("head10", "shamainbase-advanced", siblings); got == base {
		t.Fatalf("digest unchanged after base advance, want it to differ from %q", base)
	}
	changedSiblingSHA := []siblingPR{{Number: 11, HeadSHA: "sha11-pushed"}, {Number: 12, HeadSHA: "sha12"}}
	if got := computeReviewDigest("head10", "shamainbase", changedSiblingSHA); got == base {
		t.Fatalf("digest unchanged after a sibling's head SHA changed, want it to differ from %q", base)
	}
	fewerSiblings := []siblingPR{{Number: 11, HeadSHA: "sha11"}}
	if got := computeReviewDigest("head10", "shamainbase", fewerSiblings); got == base {
		t.Fatalf("digest unchanged after the sibling set shrank, want it to differ from %q", base)
	}
}

// seedVerdictCacheFixture stands up the shared fixture: selected PR #10
// (with a matching issue record, since verdict comments post to the issues
// API) plus one sibling #11.
func seedVerdictCacheFixture(t *testing.T) (root string, server *fakeGitHubServer, wantDigest string) {
	t.Helper()
	root = initDemo(t)
	server = newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10head", "shamainbase", false, nil, nil)
	server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11head", "shamainbase",
		false, nil, []fakePRFile{{path: "internal/runner/run.go", status: "modified", additions: 1, deletions: 0}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-2")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")

	wantDigest = computeReviewDigest("sha10head", "shamainbase", []siblingPR{{Number: 11, HeadSHA: "sha11head"}})
	return root, server, wantDigest
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
