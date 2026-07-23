package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestRenderRemediationStateCommentRoundTrips proves the embedded payload
// remediation-checkpoint posts (mirroring apply-verdict's verdict-json,
// applyverdict.go) survives a render/parse round trip.
func TestRenderRemediationStateCommentRoundTrips(t *testing.T) {
	s := remediationState{Cycles: 3, LastDiffDigest: "sha256:abc123"}
	comment, err := remediationStateComment(s)
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	if !strings.Contains(comment, "<!-- remediation-state:") {
		t.Fatalf("comment = %q, want an embedded remediation-state payload", comment)
	}
	got, ok := parseRemediationStateComment(comment)
	if !ok {
		t.Fatalf("parseRemediationStateComment did not find a payload in: %q", comment)
	}
	if got != s {
		t.Fatalf("parsed state = %+v, want %+v", got, s)
	}
}

// TestParseRemediationStateCommentNoPayloadIsNotFound proves an ordinary
// comment (no embedded payload — ordinary human/other-agent thread comment,
// or a PR's first pr-remediation cycle) is a clean "not found," not a parse
// error.
func TestParseRemediationStateCommentNoPayloadIsNotFound(t *testing.T) {
	if _, ok := parseRemediationStateComment("please rebase, thanks!"); ok {
		t.Fatal("parseRemediationStateComment on a plain comment: ok = true, want false")
	}
}

// remediationCheckpointServerState is a small stateful fake GitHub server
// for #364's tests: one open PR, its mutable label set, and its mutable
// comment thread (so a checkpoint run's own posted comment is visible to a
// subsequent run in the same test, letting tests exercise the durable
// cross-run counter/digest without hardcoding a digest value).
type remediationCheckpointServerState struct {
	mu sync.Mutex

	number           int
	headSHA, baseSHA string
	labels           []string
	comments         []string
	siblings         []remediationCheckpointSibling
	labelRemovalAuth string
}

type remediationCheckpointSibling struct {
	number    int
	state     string
	merged    bool
	updatedAt time.Time
	comments  []string
}

func newRemediationCheckpointServer(t *testing.T, owner, repo string, st *remediationCheckpointServerState) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		state := r.URL.Query().Get("state")
		out := make([]map[string]interface{}, 0, 1+len(st.siblings))
		if state == "" || state == "open" {
			out = append(out, map[string]interface{}{
				"number": st.number, "draft": false,
				"state":    "open",
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, st.number),
				"head":     map[string]interface{}{"ref": "goobers/impl/remediation-364", "sha": st.headSHA},
				"base":     map[string]interface{}{"ref": "main", "sha": st.baseSHA},
				"labels":   labelsJSON(st.labels),
			})
		}
		for _, sibling := range st.siblings {
			if state == "open" && sibling.state != "open" {
				continue
			}
			if state == "closed" && sibling.state != "closed" {
				continue
			}
			pr := map[string]interface{}{
				"number": sibling.number, "draft": false, "state": sibling.state,
				"updated_at": sibling.updatedAt.Format(time.RFC3339),
				"html_url":   fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, sibling.number),
				"head":       map[string]interface{}{"ref": fmt.Sprintf("goobers/impl/sibling-%d", sibling.number), "sha": fmt.Sprintf("head-%d", sibling.number)},
				"base":       map[string]interface{}{"ref": "main", "sha": st.baseSHA},
			}
			if sibling.state == "closed" {
				pr["closed_at"] = sibling.updatedAt.Format(time.RFC3339)
			}
			if sibling.merged {
				pr["merged_at"] = sibling.updatedAt.Format(time.RFC3339)
			}
			out = append(out, pr)
		}
		writeFakeJSON(w, out)
	})

	// git/ref/heads/main answers GitHubProvider.BranchTipSHA — the LIVE base
	// tip runRemediationCheckpoint records as EscalatedBaseSHA at escalation
	// (#1052), rather than the pinned pull_request.base.sha.
	mux.HandleFunc(prefix+"/git/ref/", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		writeFakeJSON(w, map[string]interface{}{"object": map[string]string{"sha": st.baseSHA}})
	})

	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/status", prefix, st.headSHA), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"state": "success", "statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc(fmt.Sprintf("%s/commits/%s/check-runs", prefix, st.headSHA), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})

	mux.HandleFunc(fmt.Sprintf("%s/issues/%d", prefix, st.number), func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		writeFakeJSON(w, map[string]interface{}{
			"number": st.number, "state": "open", "labels": labelsJSON(st.labels),
			"html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, st.number),
		})
	})

	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/labels", prefix, st.number), func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeFakeJSON(r, &body)
		st.mu.Lock()
		st.labels = append(st.labels, body.Labels...)
		st.mu.Unlock()
		writeFakeJSON(w, []map[string]string{})
	})
	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/labels/", prefix, st.number), func(w http.ResponseWriter, r *http.Request) {
		label := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("%s/issues/%d/labels/", prefix, st.number))
		st.mu.Lock()
		st.labelRemovalAuth = r.Header.Get("Authorization")
		kept := st.labels[:0]
		for _, l := range st.labels {
			if l != label {
				kept = append(kept, l)
			}
		}
		st.labels = kept
		st.mu.Unlock()
		writeFakeJSON(w, []map[string]string{})
	})

	mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, st.number), func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if r.Method == http.MethodPost {
			var body struct {
				Body string `json:"body"`
			}
			decodeFakeJSON(r, &body)
			st.comments = append(st.comments, body.Body)
			writeFakeJSON(w, map[string]interface{}{"id": len(st.comments)})
			return
		}
		out := make([]map[string]interface{}, len(st.comments))
		for i, c := range st.comments {
			out[i] = map[string]interface{}{"id": i + 1, "user": map[string]string{"login": "goobers-bot"}, "body": c, "created_at": "2026-07-15T00:00:00Z"}
		}
		writeFakeJSON(w, out)
	})
	for _, sibling := range st.siblings {
		sibling := sibling
		mux.HandleFunc(fmt.Sprintf("%s/issues/%d/comments", prefix, sibling.number), func(w http.ResponseWriter, r *http.Request) {
			out := make([]map[string]interface{}, len(sibling.comments))
			for i, comment := range sibling.comments {
				out[i] = map[string]interface{}{
					"id": i + 1, "user": map[string]string{"login": "goobers-bot"},
					"body": comment, "created_at": sibling.updatedAt.Format(time.RFC3339),
				}
			}
			writeFakeJSON(w, out)
		})
	}

	// GitHub's comment-edit endpoint is flat (repo-scoped comment IDs, not
	// nested under an issue number) — matches providers.GitHubProvider.
	// UpdateComment's PATCH target (#716's sticky-comment mechanism). This
	// fake server mints comment IDs as a 1-based index into st.comments (see
	// the POST handler above), so editing just overwrites that slot.
	mux.HandleFunc(prefix+"/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
			return
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, prefix+"/issues/comments/"))
		if err != nil {
			http.Error(w, "bad comment id", http.StatusBadRequest)
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		decodeFakeJSON(r, &body)
		st.mu.Lock()
		defer st.mu.Unlock()
		if idx < 1 || idx > len(st.comments) {
			http.Error(w, "comment not found", http.StatusNotFound)
			return
		}
		st.comments[idx-1] = body.Body
		writeFakeJSON(w, map[string]interface{}{"id": idx, "body": body.Body})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// initRemediationCheckpointRepo builds a bare origin seeded with a base
// commit on main and a PR branch carrying one further commit (pushed to
// origin), then t.Chdir's the test into a THIRD, separate clone checked out
// at main — simulating the fresh worktree remediation-checkpoint's own
// stage gets (internal/runner's buildEnvelope: continuity is keyed on the
// run's own shared branch, not on whatever an earlier stage — gather-pr-
// context, rebase-pr — locally checked out). Proves the stage's own
// re-checkout (checkoutExistingBranch) is what puts it on the PR's actual
// branch, not an accident of the test's working directory already being
// there. Returns the PR branch's base and head SHAs.
func initRemediationCheckpointRepo(t *testing.T, prBranch string) (baseSHA, headSHA string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitT(t, work, "add", "README.md")
	runGitT(t, work, "commit", "-m", "seed")
	runGitT(t, work, "push", "origin", "main")
	baseSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	runGitT(t, work, "checkout", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("pr work\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGitT(t, work, "add", "feature.txt")
	runGitT(t, work, "commit", "-m", "pr work")
	runGitT(t, work, "push", "origin", prBranch)
	headSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	// The stage's own fresh worktree: a separate clone, checked out at
	// main — nowhere near prBranch — so a passing test proves the stage's
	// own re-checkout logic, not the test's incidental working directory.
	fresh := filepath.Join(root, "fresh-worktree")
	runGitT(t, root, "clone", origin, fresh)
	t.Chdir(fresh)
	return baseSHA, headSHA
}

func remediationCheckpointEnv(t *testing.T, serverURL string, withoutCapability bool) (instanceRoot string) {
	t.Helper()
	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: serverURL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-364")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	if !withoutCapability {
		t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
		t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	}
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	return instanceRoot
}

// TestRemediationCheckpointRecordsFirstCycle is #364's headline acceptance
// for D4: a PR with no prior recorded state gets cycle 1 recorded as a new
// sticky comment, and is NOT escalated (1 cycle is nowhere near the
// liberal default budget).
func TestRemediationCheckpointRecordsFirstCycle(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{"goobers:needs-remediation"}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "cycle 1/") {
		t.Fatalf("stdout = %q, want a mention of cycle 1", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.labels {
		if l == remediationEscalatedLabel {
			t.Fatalf("labels = %v, want no merge-escalated label on a first, healthy cycle", st.labels)
		}
	}
	if len(st.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one posted (the recorded checkpoint state)", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Cycles != 1 || state.LastDiffDigest == "" {
		t.Fatalf("posted comment %q -> state=%+v ok=%v, want cycles=1 with a non-empty digest", st.comments[0], state, ok)
	}
	// #832: every recorded cycle carries the PR's head/base SHA so the next
	// cycle's rebase-aware same-diff check can tell a stall from a rebase.
	if state.HeadSHA != headSHA || state.BaseSHA != baseSHA {
		t.Fatalf("state head/base = %q/%q, want %q/%q recorded on the cycle", state.HeadSHA, state.BaseSHA, headSHA, baseSHA)
	}
}

// TestRemediationCheckpointEscalatesOnBudgetExhaustion is D4's headline
// acceptance: a PR whose prior recorded cycle count already meets --budget
// escalates (goobers:merge-escalated added, needs-remediation removed)
// rather than recording yet another cycle.
func TestRemediationCheckpointEscalatesOnBudgetExhaustion(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{Cycles: 1, LastDiffDigest: "sha256:stale-digest-from-a-different-diff"})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:   []string{"goobers:needs-remediation"},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--budget", "1", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "escalated") {
		t.Fatalf("stdout = %q, want a mention of escalation", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	found := false
	for _, l := range st.labels {
		if l == remediationEscalatedLabel {
			found = true
		}
		if l == needsRemediationLabel {
			t.Fatalf("labels = %v, want needs-remediation cleared on escalation", st.labels)
		}
	}
	if !found {
		t.Fatalf("labels = %v, want goobers:merge-escalated added", st.labels)
	}
}

// TestRemediationCheckpointEscalatesOnSameDiff is D5's headline acceptance:
// a second cycle whose diff is byte-identical to the first's escalates
// immediately, independent of the (liberal, unexhausted) budget — mirroring
// #316's in-run same-diff short-circuit.
func TestRemediationCheckpointEscalatesOnSameDiff(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{"goobers:needs-remediation"}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	// First cycle: no prior state, records cycle 1's digest.
	if code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot); code != 0 {
		t.Fatalf("first cycle: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	// Second cycle: no new commits landed since — the diff is identical —
	// so this must escalate even though the (default, liberal) budget is
	// nowhere near exhausted (cycle 2 of 10).
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("second cycle: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "byte-identical") {
		t.Fatalf("second cycle stdout = %q, want a mention of the byte-identical diff", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	found := false
	for _, l := range st.labels {
		if l == remediationEscalatedLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("labels = %v, want goobers:merge-escalated added on the same-diff repeat", st.labels)
	}
}

func TestRemediationCheckpointEscalationIncludesKnownSiblingOverlaps(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	now := time.Now().UTC()
	mergedFinding := "Sibling PR #77 concurrently changes resume.go and must be reconciled."
	openFinding := "PR #77 overlaps the same scheduler architecture."
	legacyFinding := "PR #77 must wait for this branch to land first."
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{"goobers:needs-remediation"},
		siblings: []remediationCheckpointSibling{
			{
				number: 613, state: "closed", merged: true, updatedAt: now,
				comments: []string{renderVerdictComment(apiv1.Verdict{
					Decision: apiv1.VerdictNeedsChanges,
					Findings: []apiv1.Finding{{
						Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive,
						Message: mergedFinding, Location: "cmd/goobers/resume.go",
					}},
				}), renderVerdictComment(apiv1.Verdict{
					Decision: apiv1.VerdictPass,
					Summary:  "The sibling was updated and is ready to merge.",
				})},
			},
			{
				number: 614, state: "open", updatedAt: now,
				comments: []string{renderVerdictComment(apiv1.Verdict{
					Decision: apiv1.VerdictNeedsChanges,
					Findings: []apiv1.Finding{{
						Severity: apiv1.SeverityWarning, Class: apiv1.FindingConflict,
						Message: openFinding,
					}},
				})},
			},
			{
				number: 615, state: "open", updatedAt: now,
				comments: []string{renderVerdictComment(apiv1.Verdict{
					Decision: apiv1.VerdictNeedsChanges,
					Findings: []apiv1.Finding{{
						Severity: apiv1.SeverityInfo, Class: apiv1.FindingCrossPRBlocked,
						Message: legacyFinding,
					}},
				})},
			},
		},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	if code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot); code != 0 {
		t.Fatalf("first cycle: code = %d, stderr = %q", code, stderr)
	}
	if code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot); code != 0 {
		t.Fatalf("second cycle: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 1 {
		t.Fatalf("comments = %v, want one sticky escalation comment", st.comments)
	}
	comment := st.comments[0]
	for _, want := range []string{
		"Sibling PR #613 is **merged**", mergedFinding,
		"Sibling PR #614 is **open**", openFinding,
		"Sibling PR #615 is **open**", legacyFinding,
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("escalation comment = %q, want %q", comment, want)
		}
	}
	state, ok := parseRemediationStateComment(comment)
	if !ok ||
		!strings.Contains(state.SiblingOverlapContext, "PR #613") ||
		!strings.Contains(state.SiblingOverlapContext, "PR #614") ||
		!strings.Contains(state.SiblingOverlapContext, "PR #615") {
		t.Fatalf("escalation state = %+v, ok = %v, want all persisted sibling overlaps", state, ok)
	}
}

// TestRemediationStalled is #832's core: a byte-identical diff is only a
// no-progress stall when the base has ALSO not advanced. A clean rebase onto
// newer main reproduces the same diff while advancing BaseSHA — that is
// progress, not a stall, and must not escalate.
func TestRemediationStalled(t *testing.T) {
	const digest = "sha256:abc"
	tests := []struct {
		name           string
		prior          remediationState
		digest         string
		currentBaseSHA string
		want           bool
	}{
		{
			name:           "identical diff, same base -> stalled",
			prior:          remediationState{LastDiffDigest: digest, BaseSHA: "base-1"},
			digest:         digest,
			currentBaseSHA: "base-1",
			want:           true,
		},
		{
			name:           "identical diff but base advanced (clean rebase) -> not stalled",
			prior:          remediationState{LastDiffDigest: digest, BaseSHA: "base-1"},
			digest:         digest,
			currentBaseSHA: "base-2",
			want:           false,
		},
		{
			name:           "different diff -> not stalled regardless of base",
			prior:          remediationState{LastDiffDigest: "sha256:other", BaseSHA: "base-1"},
			digest:         digest,
			currentBaseSHA: "base-1",
			want:           false,
		},
		{
			name:           "no prior digest (first cycle) -> not stalled",
			prior:          remediationState{},
			digest:         digest,
			currentBaseSHA: "base-1",
			want:           false,
		},
		{
			name:           "identical diff, prior has no BaseSHA (pre-#832 record) -> falls back to digest-only, stalled",
			prior:          remediationState{LastDiffDigest: digest},
			digest:         digest,
			currentBaseSHA: "base-2",
			want:           true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remediationStalled(tt.prior, tt.digest, tt.currentBaseSHA); got != tt.want {
				t.Fatalf("remediationStalled(%+v, %q, %q) = %v, want %v", tt.prior, tt.digest, tt.currentBaseSHA, got, tt.want)
			}
		})
	}
}

// TestRemediationCheckpointPRNoLongerOpenIsMoot proves a PR that closed/
// merged between selection and this stage running is a normal no-op (exit
// 0), not an error — mirrors apply-verdict's own D6 void-verdict shape.
func TestRemediationCheckpointPRNoLongerOpenIsMoot(t *testing.T) {
	initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 999} // no PR #77 in the list
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q, want 0 (moot)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no longer open") {
		t.Fatalf("stdout = %q, want a mention that the PR is no longer open", stdout)
	}
}

// TestRemediationCheckpointRefusesWithoutCapability proves
// remediation-checkpoint fails closed before any provider/git call when
// github:pr:write is absent.
func TestRemediationCheckpointRefusesWithoutCapability(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Chdir(t.TempDir())
	t.Setenv("GOOBERS_RUN_ID", "run-364-nocap")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	// Deliberately no GOOBERS_CRED_GITHUB_PR_WRITE set.

	code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (fail closed on missing capability)", code, stderr)
	}
}

// TestRemediationCheckpointRefusesWithoutRepoPushCapability proves
// remediation-checkpoint also fails closed when repo:push is absent — it
// re-checks-out the PR's branch itself (this stage's own fresh worktree),
// which needs the same push-scoped fetch credential checkoutExistingBranch
// uses elsewhere.
func TestRemediationCheckpointRefusesWithoutRepoPushCapability(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Chdir(t.TempDir())
	t.Setenv("GOOBERS_RUN_ID", "run-364-norepopush")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	// Deliberately no GOOBERS_CRED_REPO_PUSH set.

	code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (fail closed on missing repo:push capability)", code, stderr)
	}
}

// TestRemediationCheckpointRequiresSelectedNumber proves the
// selectedNumber input is mandatory (mirrors apply-verdict/gather-pr-
// context's own required-input contract).
func TestRemediationCheckpointRequiresSelectedNumber(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Chdir(t.TempDir())
	t.Setenv("GOOBERS_RUN_ID", "run-364-noinput")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	// Deliberately no GOOBERS_INPUT_SELECTEDNUMBER set.

	code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (selectedNumber required)", code, stderr)
	}
}
