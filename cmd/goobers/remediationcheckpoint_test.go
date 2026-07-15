package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
}

func newRemediationCheckpointServer(t *testing.T, owner, repo string, st *remediationCheckpointServerState) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		writeFakeJSON(w, []map[string]interface{}{
			{
				"number": st.number, "draft": false,
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, st.number),
				"head":     map[string]interface{}{"ref": "goobers/impl/remediation-364", "sha": st.headSHA},
				"base":     map[string]interface{}{"ref": "main", "sha": st.baseSHA},
				"labels":   labelsJSON(st.labels),
			},
		})
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

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// initRemediationCheckpointRepo builds a local (no-remote-needed — diffDigest
// only reads local history) git repo with a base commit and one further
// commit on top, mirroring the diff a real pr-remediation push would have
// produced. Returns the base commit's SHA; HEAD is the tip. t.Chdir's the
// test into it.
func initRemediationCheckpointRepo(t *testing.T) (baseSHA string) {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.name", "seed")
	runGitT(t, dir, "config", "user.email", "seed@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitT(t, dir, "add", "README.md")
	runGitT(t, dir, "commit", "-m", "seed")
	baseSHA = strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("pr work\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGitT(t, dir, "add", "feature.txt")
	runGitT(t, dir, "commit", "-m", "pr work")

	t.Chdir(dir)
	return baseSHA
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
	}
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	return instanceRoot
}

// TestRemediationCheckpointRecordsFirstCycle is #364's headline acceptance
// for D4: a PR with no prior recorded state gets cycle 1 recorded as a new
// sticky comment, and is NOT escalated (1 cycle is nowhere near the
// liberal default budget).
func TestRemediationCheckpointRecordsFirstCycle(t *testing.T) {
	baseSHA := initRemediationCheckpointRepo(t)
	st := &remediationCheckpointServerState{number: 77, headSHA: "head-sha-1", baseSHA: baseSHA, labels: []string{"goobers:needs-remediation"}}
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
}

// TestRemediationCheckpointEscalatesOnBudgetExhaustion is D4's headline
// acceptance: a PR whose prior recorded cycle count already meets --budget
// escalates (goobers:merge-escalated added, needs-remediation removed)
// rather than recording yet another cycle.
func TestRemediationCheckpointEscalatesOnBudgetExhaustion(t *testing.T) {
	baseSHA := initRemediationCheckpointRepo(t)
	priorComment, err := remediationStateComment(remediationState{Cycles: 1, LastDiffDigest: "sha256:stale-digest-from-a-different-diff"})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: "head-sha-2", baseSHA: baseSHA,
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
	baseSHA := initRemediationCheckpointRepo(t)
	st := &remediationCheckpointServerState{number: 77, headSHA: "head-sha-3", baseSHA: baseSHA, labels: []string{"goobers:needs-remediation"}}
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

// TestRemediationCheckpointPRNoLongerOpenIsMoot proves a PR that closed/
// merged between selection and this stage running is a normal no-op (exit
// 0), not an error — mirrors apply-verdict's own D6 void-verdict shape.
func TestRemediationCheckpointPRNoLongerOpenIsMoot(t *testing.T) {
	initRemediationCheckpointRepo(t)
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
	t.Setenv("GOOBERS_RUN_ID", "run-364-nocap")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	// Deliberately no GOOBERS_CRED_GITHUB_PR_WRITE set.

	code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (fail closed on missing capability)", code, stderr)
	}
}

// TestRemediationCheckpointRequiresSelectedNumber proves the
// selectedNumber input is mandatory (mirrors apply-verdict/gather-pr-
// context's own required-input contract).
func TestRemediationCheckpointRequiresSelectedNumber(t *testing.T) {
	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-364-noinput")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	// Deliberately no GOOBERS_INPUT_SELECTEDNUMBER set.

	code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (selectedNumber required)", code, stderr)
	}
}
