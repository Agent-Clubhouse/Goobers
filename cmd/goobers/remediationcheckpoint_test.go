package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

// TestRenderRemediationStateCommentRoundTrips proves the embedded payload
// remediation-checkpoint posts (mirroring apply-verdict's verdict-json,
// applyverdict.go) survives a render/parse round trip.
func TestRenderRemediationStateCommentRoundTrips(t *testing.T) {
	s := remediationState{
		Cycles: 3, AttemptsByCause: remediationAttempts{Conflict: 2, FailingCI: 1},
		LastDiffDigest: "sha256:abc123",
	}
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
	if !reflect.DeepEqual(got, s) {
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

	number             int
	headSHA, baseSHA   string
	listedHeadSHA      string
	liveBaseSHA        string
	state              string
	body               string
	merged             bool
	terminalOnComments bool
	mergeOnComments    bool
	labels             []string
	comments           []string
	// commentCreatedAt is optional, index-aligned with comments: an empty (or
	// short) entry serves the default timestamp so existing tests are
	// unaffected; the sentinel "-" omits created_at entirely (a comment with
	// no timestamp), which the human-comment watermark tests use.
	commentCreatedAt    []string
	files               []providers.ChangedFile
	siblings            []remediationCheckpointSibling
	labelRemovalAuth    string
	pullListRequests    int
	pullReadStatus      int
	deleteCommentOnEdit bool
	headAfterComments   string
	baseAfterComments   string
}

type remediationCheckpointSibling struct {
	number    int
	state     string
	merged    bool
	updatedAt time.Time
	comments  []string
	files     []providers.ChangedFile
	mergeSHA  string
}

func newRemediationCheckpointServer(t *testing.T, owner, repo string, st *remediationCheckpointServerState) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + owner + "/" + repo
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.pullListRequests++
		state := r.URL.Query().Get("state")
		currentState := st.state
		if currentState == "" {
			currentState = "open"
		}
		listedHeadSHA := st.headSHA
		if st.listedHeadSHA != "" {
			listedHeadSHA = st.listedHeadSHA
		}
		out := make([]map[string]interface{}, 0, 1+len(st.siblings))
		if (state == "" || state == "open") && currentState == "open" {
			out = append(out, map[string]interface{}{
				"number": st.number, "draft": false,
				"state":    currentState,
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, st.number),
				"head":     map[string]interface{}{"ref": "goobers/impl/remediation-364", "sha": listedHeadSHA},
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
				pr["merge_commit_sha"] = sibling.mergeSHA
			}
			out = append(out, pr)
		}
		writeFakeJSON(w, out)
	})

	mux.HandleFunc(fmt.Sprintf("%s/pulls/%d", prefix, st.number), func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.pullReadStatus != 0 {
			http.Error(w, "pull request read failed", st.pullReadStatus)
			return
		}
		state := st.state
		if state == "" {
			state = "open"
		}
		writeFakeJSON(w, map[string]interface{}{
			"number": st.number, "draft": false, "state": state, "merged": st.merged,
			"body":     st.body,
			"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, st.number),
			"head":     map[string]interface{}{"ref": "goobers/impl/remediation-364", "sha": st.headSHA},
			"base":     map[string]interface{}{"ref": "main", "sha": st.baseSHA},
			"labels":   labelsJSON(st.labels),
		})
	})
	mux.HandleFunc(fmt.Sprintf("%s/pulls/%d/files", prefix, st.number), func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, githubChangedFiles(st.files))
	})

	// git/ref/heads/main answers GitHubProvider.BranchTipSHA — the LIVE base
	// tip runRemediationCheckpoint records as EscalatedBaseSHA at escalation
	// (#1052), rather than the pinned pull_request.base.sha.
	mux.HandleFunc(prefix+"/git/ref/", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		sha := st.liveBaseSHA
		if sha == "" {
			sha = st.baseSHA
		}
		writeFakeJSON(w, map[string]interface{}{"object": map[string]string{"sha": sha}})
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
		if st.terminalOnComments {
			st.state = "closed"
			st.merged = st.mergeOnComments
		}
		if st.headAfterComments != "" {
			st.headSHA = st.headAfterComments
			st.headAfterComments = ""
		}
		if st.baseAfterComments != "" {
			st.baseSHA = st.baseAfterComments
			st.baseAfterComments = ""
		}
		out := make([]map[string]interface{}, len(st.comments))
		for i, c := range st.comments {
			entry := map[string]interface{}{"id": i + 1, "user": map[string]string{"login": "goobers-bot"}, "body": c}
			createdAt := "2026-07-15T00:00:00Z"
			if i < len(st.commentCreatedAt) {
				createdAt = st.commentCreatedAt[i]
			}
			if createdAt != "-" {
				entry["created_at"] = createdAt
			}
			out[i] = entry
		}
		writeFakeJSON(w, out)
	})
	for _, sibling := range st.siblings {
		sibling := sibling
		mux.HandleFunc(fmt.Sprintf("%s/pulls/%d/files", prefix, sibling.number), func(w http.ResponseWriter, r *http.Request) {
			writeFakeJSON(w, githubChangedFiles(sibling.files))
		})
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
		if st.deleteCommentOnEdit {
			st.deleteCommentOnEdit = false
			st.comments = append(st.comments[:idx-1], st.comments[idx:]...)
			http.Error(w, "comment deleted", http.StatusNotFound)
			return
		}
		st.comments[idx-1] = body.Body
		writeFakeJSON(w, map[string]interface{}{"id": idx, "body": body.Body})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func githubChangedFiles(files []providers.ChangedFile) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(files))
	for _, file := range files {
		out = append(out, map[string]interface{}{
			"filename": file.Path, "previous_filename": file.PreviousPath, "status": file.Status,
			"additions": file.Additions, "deletions": file.Deletions, "patch": file.Patch,
		})
	}
	return out
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

func initStructuralCollisionCheckpointRepo(t *testing.T, prBranch string) (baseSHA, headSHA, mergeSHA string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	runGitT(t, root, "init", "--bare", "-b", "main", origin)

	work := filepath.Join(root, "work")
	runGitT(t, root, "clone", origin, work)
	runGitT(t, work, "config", "user.name", "seed")
	runGitT(t, work, "config", "user.email", "seed@example.com")
	oldSource := "package status\n\nfunc runStatus() {\n\tloadStatus()\n\tloadWarnings()\n\trenderHeader()\n\trenderBody()\n\trenderWarnings()\n\trenderFooter()\n\tflushOutput()\n\trecordFrame()\n}\n"
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte(oldSource), 0o644); err != nil {
		t.Fatalf("write old status.go: %v", err)
	}
	runGitT(t, work, "add", "status.go")
	runGitT(t, work, "commit", "-m", "seed status")
	runGitT(t, work, "push", "origin", "main")
	baseSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	runGitT(t, work, "checkout", "-b", prBranch)
	prSource := strings.Replace(oldSource, "\trenderBody()\n", "\trenderBodyWithWarnings()\n", 1)
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte(prSource), 0o644); err != nil {
		t.Fatalf("write PR status.go: %v", err)
	}
	runGitT(t, work, "commit", "-am", "add status warnings")
	runGitT(t, work, "push", "origin", prBranch)
	headSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	runGitT(t, work, "checkout", "main")
	newSource := "package status\n\nfunc runStatus() {\n\tframe := buildStatusFrame()\n\trenderStatusFrame(frame)\n\trecordFrame(frame)\n}\n\nfunc buildStatusFrame() statusFrame {\n\treturn statusFrame{}\n}\n"
	if err := os.WriteFile(filepath.Join(work, "status.go"), []byte(newSource), 0o644); err != nil {
		t.Fatalf("write restructured status.go: %v", err)
	}
	runGitT(t, work, "commit", "-am", "restructure status rendering")
	runGitT(t, work, "push", "origin", "main")
	mergeSHA = strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	fresh := filepath.Join(root, "fresh-worktree")
	runGitT(t, root, "clone", origin, fresh)
	t.Chdir(fresh)
	return baseSHA, headSHA, mergeSHA
}

func remediationCheckpointEnv(t *testing.T, serverURL string, withoutCapability bool) (instanceRoot string) {
	t.Helper()
	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: serverURL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-364")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_REPO_PROVIDER", "github")
	t.Setenv("GOOBERS_REPO_OWNER", "your-org")
	t.Setenv("GOOBERS_REPO_NAME", "your-repo")
	if !withoutCapability {
		t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
		t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	}
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "substantive")
	t.Setenv("GOOBERS_INPUT_CONFLICTBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_SUBSTANTIVEBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_FAILINGCIBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_SIBLINGOVERLAPBUDGET", "2")
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
	if !strings.Contains(stdout, "cycle 1") {
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
	if !ok || state.Cycles != 1 || state.AttemptsByCause.Substantive != 1 || state.LastDiffDigest == "" {
		t.Fatalf("posted comment %q -> state=%+v ok=%v, want cycle 1 with one substantive attempt and a non-empty digest", st.comments[0], state, ok)
	}
	// #832: every recorded cycle carries the PR's head/base SHA so the next
	// cycle's rebase-aware same-diff check can tell a stall from a rebase.
	if state.HeadSHA != headSHA || state.BaseSHA != baseSHA {
		t.Fatalf("state head/base = %q/%q, want %q/%q recorded on the cycle", state.HeadSHA, state.BaseSHA, headSHA, baseSHA)
	}
}

// TestRemediationCheckpointEscalatesExhaustedConflictCause is #953's headline
// acceptance: two admitted conflict attempts exhaust only the conflict budget,
// and the escalation names that cause.
func TestRemediationCheckpointEscalatesExhaustedConflictCause(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 2, AttemptsByCause: remediationAttempts{Conflict: 2},
		LastDiffDigest: "sha256:stale-digest-from-a-different-diff",
	})
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
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "conflict")
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `cause "conflict" exhausted its budget`) {
		t.Fatalf("stdout = %q, want conflict-specific budget escalation", stdout)
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
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.AttemptsByCause.Conflict != 2 || state.AttemptsByCause.FailingCI != 0 {
		t.Fatalf("escalated state = %+v, ok = %v, want only two conflict attempts consumed", state, ok)
	}
	if state.EscalationOutcome != remediationOutcomeBudgetExhausted || !state.RemediationAttempted ||
		!reflect.DeepEqual(state.AttemptedCauses, []remediationCause{remediationCauseConflict}) {
		t.Fatalf("escalation evidence = outcome %q, attempted %t, causes %v", state.EscalationOutcome, state.RemediationAttempted, state.AttemptedCauses)
	}
	result := readCheckpointResult(t, "checkpoint-result.json")
	if result["escalationOutcome"] != string(remediationOutcomeBudgetExhausted) ||
		result["remediationAttempted"] != "true" || result["attemptedCauses"] != "conflict" {
		t.Fatalf("checkpoint result escalation evidence = %v", result)
	}
}

func TestRemediationCheckpointClearsSelfHealedEscalationForIndependentCause(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 2, AttemptsByCause: remediationAttempts{Conflict: 2},
		LastDiffDigest: "sha256:stale-digest-from-a-different-diff",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:   []string{needsRemediationLabel},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "conflict")

	if code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot); code != 0 {
		t.Fatalf("conflict escalation: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	runGitT(t, ".", "config", "user.name", "remediator")
	runGitT(t, ".", "config", "user.email", "remediator@example.com")
	if err := os.WriteFile("ci-fix.txt", []byte("repair failing CI\n"), 0o644); err != nil {
		t.Fatalf("write CI fix: %v", err)
	}
	runGitT(t, ".", "add", "ci-fix.txt")
	runGitT(t, ".", "commit", "-m", "repair failing CI")
	runGitT(t, ".", "push", "origin", "HEAD")
	advancedHeadSHA := strings.TrimSpace(runGitOutputT(t, ".", "rev-parse", "HEAD"))

	st.mu.Lock()
	st.headSHA = advancedHeadSHA
	st.mu.Unlock()
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "failing-ci")

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("failing-CI checkpoint: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "failing-ci=1") {
		t.Fatalf("stdout = %q, want the independent failing-CI allowance recorded", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for _, label := range st.labels {
		if label == remediationEscalatedLabel {
			t.Fatalf("labels = %v, want stale merge-escalated cleared after the head advanced", st.labels)
		}
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Escalated || state.HeadSHA != advancedHeadSHA ||
		state.AttemptsByCause.Conflict != 2 || state.AttemptsByCause.FailingCI != 1 {
		t.Fatalf("checkpoint state = %+v, ok = %v, want conflict=2 and failing-ci=1 on the advanced head", state, ok)
	}
}

// TestRemediationCheckpointEscalatesImmediatelyOnPolicyExcluded is #941/
// PRR-6's checkpoint-side acceptance: rebase-pr's policyExcluded/
// policyExcludedReason inputs escalate this cycle immediately, exactly like
// an explicit --escalate, even on cycle 1 (nowhere near budget) with no
// stalled diff — no agent ever ran on this cycle, so the ordinary D4/D5
// checks have nothing to gain by running first.
func TestRemediationCheckpointEscalatesImmediatelyOnPolicyExcluded(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{"goobers:needs-remediation"}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	t.Setenv("GOOBERS_INPUT_POLICYEXCLUDED", "true")
	t.Setenv("GOOBERS_INPUT_POLICYEXCLUDEDREASON", `remediation policy "conflict" excludes the only detected cause(s) (substantive)`)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
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
	if len(st.comments) != 1 || !strings.Contains(st.comments[0], "excludes the only detected cause") {
		t.Fatalf("comments = %v, want the escalation reason to name the policy exclusion", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.EscalationOutcome != remediationOutcomePolicyExcluded ||
		state.RemediationAttempted || len(state.AttemptedCauses) != 0 {
		t.Fatalf("policy-excluded escalation state = %+v, ok = %v", state, ok)
	}
	result := readCheckpointResult(t, "checkpoint-result.json")
	if result["escalationOutcome"] != string(remediationOutcomePolicyExcluded) ||
		result["remediationAttempted"] != "false" || result["attemptedCauses"] != "" {
		t.Fatalf("policy-excluded checkpoint result = %v", result)
	}
}

func TestRemediationCheckpointHaltsWithoutObservedCause(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel},
	}

	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	resultFile := filepath.Join(t.TempDir(), "checkpoint-result.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "")

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := readCheckpointResult(t, resultFile)["continueRemediation"]; got != "false" {
		t.Fatalf("continueRemediation = %q, want false without an observed cause", got)
	}
	if !strings.Contains(stdout, "without consuming an allowance") {
		t.Fatalf("stdout = %q, want explicit no-cause halt", stdout)
	}
	if len(st.comments) != 1 {
		t.Fatalf("comments = %v, want a persisted no-cause checkpoint for independent stall detection", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Cycles != 1 || state.LastDiffDigest == "" || state.AttemptsByCause != (remediationAttempts{}) {
		t.Fatalf("no-cause state = %+v, ok=%v, want cycle and digest with unchanged counters", state, ok)
	}

	code, stdout, stderr = runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("repeat: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "byte-identical") {
		t.Fatalf("repeat stdout = %q, want independent same-diff escalation", stdout)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, want merge-escalated on repeated no-cause failure", st.labels)
	}
}

func TestRemediationCheckpointWaitsForSiblingWithoutConsumingBudget(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-sibling-wait")
	priorComment, err := remediationStateComment(remediationState{
		Cycles:            2,
		AttemptsByCause:   remediationAttempts{SiblingOverlap: 2},
		LastDiffDigest:    "sha256:prior",
		HeadSHA:           headSHA,
		BaseSHA:           baseSHA,
		LastSeenCommentAt: "2026-08-09T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:   []string{blockedOnSiblingLabel, needsRemediationLabel},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	resultFile := filepath.Join(t.TempDir(), "checkpoint-result.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", string(remediationCauseSiblingOverlap))

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "waiting without consuming remediation budget") {
		t.Fatalf("stdout = %q, want sequencing wait", stdout)
	}
	if got := readCheckpointResult(t, resultFile)["continueRemediation"]; got != "false" {
		t.Fatalf("continueRemediation = %q, want false", got)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, sequencing-only wait must not escalate", st.labels)
	}
	if hasAnyLabel(st.labels, []string{needsRemediationLabel}) {
		t.Fatalf("labels = %v, sequencing-only wait retained stale needs-remediation label", st.labels)
	}
	if len(st.comments) != 1 || st.comments[0] != priorComment {
		t.Fatalf("comments changed during sequencing-only wait: %v", st.comments)
	}
}

func TestSequencingOnlyCheckpointWaitRequiresNoIndependentCause(t *testing.T) {
	labels := []string{blockedOnSiblingLabel}
	if !sequencingOnlyCheckpointWait(labels, nil) {
		t.Fatal("blocked PR with no independent cause was not parked")
	}
	if !sequencingOnlyCheckpointWait(labels, []remediationCause{remediationCauseSiblingOverlap}) {
		t.Fatal("sibling-overlap-only PR was not parked")
	}
	for _, cause := range []remediationCause{
		remediationCauseConflict,
		remediationCauseSubstantive,
		remediationCauseFailingCI,
		remediationCauseHumanComment,
	} {
		if sequencingOnlyCheckpointWait(labels, []remediationCause{remediationCauseSiblingOverlap, cause}) {
			t.Fatalf("independent cause %q was treated as sequencing-only", cause)
		}
	}
}

func TestRemediationCauseBudgetsAreIndependent(t *testing.T) {
	budgets := remediationBudgets{
		Conflict: 2, Substantive: 2, FailingCI: 2, SiblingOverlap: 2, HumanComment: 2,
	}

	var attempts remediationAttempts
	for _, cause := range remediationCauseOrder {
		if exhausted, ok := exhaustedRemediationCause(attempts, []remediationCause{cause}, budgets); ok {
			t.Fatalf("distinct cause %q unexpectedly exhausted by attempts %+v (reported %q)", cause, attempts, exhausted)
		}
		attempts.increment(cause)
	}
	for _, cause := range remediationCauseOrder {
		if got := attempts.forCause(cause); got != 1 {
			t.Fatalf("%s attempts = %d, want 1 after four distinct problems", cause, got)
		}
	}

	attempts.increment(remediationCauseConflict)
	if cause, ok := exhaustedRemediationCause(attempts, []remediationCause{remediationCauseConflict}, budgets); !ok || cause != remediationCauseConflict {
		t.Fatalf("two conflict attempts: exhausted cause = %q, ok = %v, want conflict", cause, ok)
	}
	if cause, ok := exhaustedRemediationCause(attempts, []remediationCause{remediationCauseFailingCI}, budgets); ok {
		t.Fatalf("later failing-CI cause exhausted by conflict attempts: cause = %q, attempts = %+v", cause, attempts)
	}
}

func TestDeclaredRemediationBudgetsReadWorkflowInputs(t *testing.T) {
	t.Setenv("GOOBERS_INPUT_CONFLICTBUDGET", "1")
	t.Setenv("GOOBERS_INPUT_SUBSTANTIVEBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_FAILINGCIBUDGET", "3")
	t.Setenv("GOOBERS_INPUT_SIBLINGOVERLAPBUDGET", "4")
	t.Setenv("GOOBERS_INPUT_HUMANCOMMENTBUDGET", "5")

	got, err := declaredRemediationBudgets(0)
	if err != nil {
		t.Fatalf("declaredRemediationBudgets: %v", err)
	}
	want := remediationBudgets{Conflict: 1, Substantive: 2, FailingCI: 3, SiblingOverlap: 4, HumanComment: 5}
	if got != want {
		t.Fatalf("budgets = %+v, want workflow inputs %+v", got, want)
	}
}

// TestDeclaredRemediationBudgetsHumanCommentDefaultsWhenUndeclared covers the
// backward-compat contract: an undeclared humanCommentBudget must fall back to
// the default (not error, unlike the four legacy budgets), while a non-empty
// invalid value is still a hard error.
func TestDeclaredRemediationBudgetsHumanCommentDefaultsWhenUndeclared(t *testing.T) {
	t.Setenv("GOOBERS_INPUT_CONFLICTBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_SUBSTANTIVEBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_FAILINGCIBUDGET", "2")
	t.Setenv("GOOBERS_INPUT_SIBLINGOVERLAPBUDGET", "2")

	t.Setenv("GOOBERS_INPUT_HUMANCOMMENTBUDGET", "")
	got, err := declaredRemediationBudgets(0)
	if err != nil {
		t.Fatalf("declaredRemediationBudgets with undeclared humanCommentBudget: %v", err)
	}
	if got.HumanComment != defaultHumanCommentBudget {
		t.Fatalf("HumanComment budget = %d, want default %d when undeclared", got.HumanComment, defaultHumanCommentBudget)
	}

	for _, raw := range []string{"not-a-number", "0", "-1"} {
		t.Setenv("GOOBERS_INPUT_HUMANCOMMENTBUDGET", raw)
		if _, err := declaredRemediationBudgets(0); err == nil {
			t.Fatalf("declaredRemediationBudgets accepted invalid humanCommentBudget %q", raw)
		}
	}
}

func TestDeclaredRemediationBudgetsRejectMissingOrInvalidInputs(t *testing.T) {
	for _, raw := range []string{"", "not-a-number", "0", "-1"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("GOOBERS_INPUT_CONFLICTBUDGET", raw)
			t.Setenv("GOOBERS_INPUT_SUBSTANTIVEBUDGET", "2")
			t.Setenv("GOOBERS_INPUT_FAILINGCIBUDGET", "2")
			t.Setenv("GOOBERS_INPUT_SIBLINGOVERLAPBUDGET", "2")
			if _, err := declaredRemediationBudgets(0); err == nil {
				t.Fatalf("declaredRemediationBudgets accepted conflictBudget %q", raw)
			}
		})
	}
}

func TestRemediationCheckpointBudgetsSiblingOverlapSeparately(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 2, AttemptsByCause: remediationAttempts{SiblingOverlap: 2},
		LastDiffDigest: "sha256:stale-digest-from-a-different-diff",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:   []string{needsRemediationLabel},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "sibling-overlap")

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `cause "sibling-overlap" exhausted its budget`) {
		t.Fatalf("stdout = %q, want sibling-overlap-specific budget escalation", stdout)
	}
}

// TestParseRemediationCausesAcceptsHumanComment proves the new cause is a
// recognized value and sorts last in remediationCauseOrder.
func TestParseRemediationCausesAcceptsHumanComment(t *testing.T) {
	got, err := parseRemediationCauses("human-comment,conflict")
	if err != nil {
		t.Fatalf("parseRemediationCauses: %v", err)
	}
	want := []remediationCause{remediationCauseConflict, remediationCauseHumanComment}
	if len(got) != len(want) {
		t.Fatalf("causes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("causes = %v, want %v (human-comment ordered last)", got, want)
		}
	}
}

// TestRemediationCheckpointBudgetsHumanCommentSeparately mirrors the
// sibling-overlap budget test: two prior human-comment attempts with a budget
// of 2 escalates naming the cause, budget-exhausted, and leaves the other
// counters untouched.
func TestRemediationCheckpointBudgetsHumanCommentSeparately(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 2, AttemptsByCause: remediationAttempts{HumanComment: 2},
		LastDiffDigest: "sha256:stale-digest-from-a-different-diff",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:   []string{needsRemediationLabel},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "human-comment")
	t.Setenv("GOOBERS_INPUT_HUMANCOMMENTBUDGET", "2")

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `cause "human-comment" exhausted its budget`) {
		t.Fatalf("stdout = %q, want human-comment-specific budget escalation", stdout)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, want merge-escalated on human-comment budget exhaustion", st.labels)
	}
	state, ok := parseRemediationStateComment(st.comments[len(st.comments)-1])
	if !ok {
		t.Fatalf("no state comment recorded: %v", st.comments)
	}
	if state.AttemptsByCause != (remediationAttempts{HumanComment: 2}) {
		t.Fatalf("attempts = %+v, want only HumanComment:2 (other counters untouched)", state.AttemptsByCause)
	}
}

// TestRemediationCheckpointContinuesOnHumanCommentCause is the healthy side: a
// human-comment cause under budget records an advanced cycle, increments only
// the human-comment counter, and signals continueRemediation.
func TestRemediationCheckpointContinuesOnHumanCommentCause(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	resultFile := filepath.Join(t.TempDir(), "checkpoint-result.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "human-comment")

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := readCheckpointResult(t, resultFile)["continueRemediation"]; got != "true" {
		t.Fatalf("continueRemediation = %q, want true for a human-comment cause under budget", got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	state, ok := parseRemediationStateComment(st.comments[len(st.comments)-1])
	if !ok || state.AttemptsByCause.HumanComment != 1 {
		t.Fatalf("recorded state = %+v ok=%v, want HumanComment attempt incremented to 1", state, ok)
	}
}

// TestRemediationCheckpointRecordsCommentWatermark proves the recorded
// LastSeenCommentAt is the newest comment timestamp seen this cycle, and that
// an untimestamped thread carries the prior watermark forward rather than
// dropping it.
func TestRemediationCheckpointRecordsCommentWatermark(t *testing.T) {
	t.Run("records the newest comment timestamp", func(t *testing.T) {
		baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
		priorComment, err := remediationStateComment(remediationState{
			Cycles: 1, LastSeenCommentAt: "2026-07-18T00:00:00Z",
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		st := &remediationCheckpointServerState{
			number: 77, headSHA: headSHA, baseSHA: baseSHA,
			labels:           []string{needsRemediationLabel},
			comments:         []string{priorComment, "please look", "and this too"},
			commentCreatedAt: []string{"2026-07-19T00:00:00Z", "2026-07-20T00:00:00Z", "2026-07-21T00:00:00Z"},
		}
		server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
		instanceRoot := remediationCheckpointEnv(t, server.URL, false)

		code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		state, ok := parseRemediationStateComment(st.comments[0])
		if !ok {
			t.Fatalf("state comment not parseable: %q", st.comments[0])
		}
		if state.LastSeenCommentAt != "2026-07-21T00:00:00Z" {
			t.Fatalf("LastSeenCommentAt = %q, want the newest served timestamp 2026-07-21T00:00:00Z", state.LastSeenCommentAt)
		}
	})

	t.Run("untimestamped thread carries the prior watermark forward", func(t *testing.T) {
		baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
		priorComment, err := remediationStateComment(remediationState{
			Cycles: 1, LastSeenCommentAt: "2026-07-15T00:00:00Z",
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		st := &remediationCheckpointServerState{
			number: 77, headSHA: headSHA, baseSHA: baseSHA,
			labels:           []string{needsRemediationLabel},
			comments:         []string{priorComment},
			commentCreatedAt: []string{"-"}, // no created_at served
		}
		server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
		instanceRoot := remediationCheckpointEnv(t, server.URL, false)

		code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		state, ok := parseRemediationStateComment(st.comments[0])
		if !ok {
			t.Fatalf("state comment not parseable: %q", st.comments[0])
		}
		if state.LastSeenCommentAt != "2026-07-15T00:00:00Z" {
			t.Fatalf("LastSeenCommentAt = %q, want the prior watermark carried forward", state.LastSeenCommentAt)
		}
	})
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
	// nowhere near exhausting its cause budget.
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
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.EscalationOutcome != remediationOutcomeDidNotConverge ||
		!state.RemediationAttempted ||
		!reflect.DeepEqual(state.AttemptedCauses, []remediationCause{remediationCauseSubstantive}) {
		t.Fatalf("same-diff escalation evidence = %+v, ok = %v", state, ok)
	}
}

const structuralCollisionCurrentPatch = `@@ -1,5 +1,5 @@ func runStatus() {
-	printStatus()
+	printStatusWithWarnings()
 }`

const structuralCollisionSiblingPatch = `@@ -1,13 +1,9 @@ func runStatus() {
-	loadStatus()
-	loadWarnings()
-	renderHeader()
-	renderBody()
-	renderWarnings()
-	renderFooter()
-	flushOutput()
-	recordFrame()
+	frame := buildStatusFrame()
+	renderStatusFrame(frame)
+	recordFrame(frame)
 }
+
+func buildStatusFrame() statusFrame {
+	return statusFrame{}
+}`

func TestRemediationCheckpointHydratesMissingSiblingPatchAndEscalatesFirstStructuralCollision(t *testing.T) {
	baseSHA, headSHA, mergeSHA := initStructuralCollisionCheckpointRepo(t, "goobers/impl/remediation-364")
	now := time.Now().UTC()
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		liveBaseSHA: mergeSHA, labels: []string{needsRemediationLabel},
		files: []providers.ChangedFile{{
			Path: "status.go", Status: "modified", Patch: structuralCollisionCurrentPatch,
		}},
		siblings: []remediationCheckpointSibling{{
			number: 609, state: "closed", merged: true, updatedAt: now,
			mergeSHA: mergeSHA,
			files: []providers.ChangedFile{{
				Path: "status.go", Status: "modified",
			}},
		}},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	setStructuralCollisionInputs(t, headSHA, mergeSHA)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "substantially restructured") {
		t.Fatalf("stdout = %q, want structural-collision escalation", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 1 {
		t.Fatalf("comments = %v, want one sticky escalation comment", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || !state.Escalated || state.Cycles != 1 {
		t.Fatalf("first checkpoint state = %+v, ok = %v, want an escalation on cycle 1", state, ok)
	}
	if state.EscalationOutcome != remediationOutcomeDidNotConverge ||
		!state.RemediationAttempted ||
		!reflect.DeepEqual(state.AttemptedCauses, []remediationCause{remediationCauseConflict}) {
		t.Fatalf("structural-collision escalation evidence = %+v", state)
	}
	result := readCheckpointResult(t, "checkpoint-result.json")
	if result["escalationOutcome"] != string(remediationOutcomeDidNotConverge) ||
		result["remediationAttempted"] != "true" || result["attemptedCauses"] != "conflict" {
		t.Fatalf("structural-collision checkpoint result = %v", result)
	}
	for _, want := range []string{
		"Same-function structural collision",
		"PR #77 relevant hunk",
		"printStatusWithWarnings",
		"Merged sibling PR #609 relevant hunk",
		"buildStatusFrame",
	} {
		if !strings.Contains(st.comments[0], want) {
			t.Fatalf("escalation comment = %q, want %q", st.comments[0], want)
		}
	}
	for _, label := range st.labels {
		if label == needsRemediationLabel {
			t.Fatalf("labels = %v, want needs-remediation cleared", st.labels)
		}
	}
}

func TestRemediationCheckpointEscalatesForStructuralSiblingMergedBeforeLookback(t *testing.T) {
	now := time.Now().UTC()
	oldBaseTime := now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	t.Setenv("GIT_AUTHOR_DATE", oldBaseTime)
	t.Setenv("GIT_COMMITTER_DATE", oldBaseTime)
	baseSHA, headSHA, mergeSHA := initStructuralCollisionCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		liveBaseSHA: mergeSHA, labels: []string{needsRemediationLabel},
		files: []providers.ChangedFile{{
			Path: "status.go", Status: "modified", Patch: structuralCollisionCurrentPatch,
		}},
		siblings: []remediationCheckpointSibling{{
			number: 609, state: "closed", merged: true,
			updatedAt: now.Add(-45 * 24 * time.Hour), mergeSHA: mergeSHA,
			files: []providers.ChangedFile{{
				Path: "status.go", Status: "modified", Patch: structuralCollisionSiblingPatch,
			}},
		}},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	setStructuralCollisionInputs(t, headSHA, mergeSHA)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "substantially restructured") {
		t.Fatalf("stdout = %q, want structural-collision escalation for sibling merged before fixed lookback", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || !state.Escalated || state.Cycles != 1 {
		t.Fatalf("first checkpoint state = %+v, ok = %v, want escalation for stale-base sibling", state, ok)
	}
}

func setStructuralCollisionInputs(t *testing.T, attemptedHeadSHA, rebaseBaseSHA string) {
	t.Helper()
	locations, err := json.Marshal([]rebaseConflictLocation{{
		Path: "status.go", Scope: "func runStatus() {",
	}})
	if err != nil {
		t.Fatalf("marshal conflict locations: %v", err)
	}
	t.Setenv("GOOBERS_INPUT_CONFLICT", "true")
	t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "conflict")
	t.Setenv("GOOBERS_INPUT_CONFLICTLOCATIONS", string(locations))
	t.Setenv("GOOBERS_INPUT_ATTEMPTEDHEADSHA", attemptedHeadSHA)
	t.Setenv("GOOBERS_INPUT_REBASEBASESHA", rebaseBaseSHA)
}

func TestRemediationCheckpointIgnoresStructuralSiblingAlreadyInSelectedBase(t *testing.T) {
	baseSHA, headSHA, liveBaseSHA := initStructuralCollisionCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA, liveBaseSHA: liveBaseSHA,
		labels: []string{needsRemediationLabel},
		files: []providers.ChangedFile{{
			Path: "status.go", Status: "modified", Patch: structuralCollisionCurrentPatch,
		}},
		siblings: []remediationCheckpointSibling{{
			number: 608, state: "closed", merged: true, updatedAt: time.Now().UTC(),
			mergeSHA: baseSHA,
			files: []providers.ChangedFile{{
				Path: "status.go", Status: "modified", Patch: structuralCollisionSiblingPatch,
			}},
		}},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	setStructuralCollisionInputs(t, headSHA, liveBaseSHA)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "recorded checkpoint") {
		t.Fatalf("stdout = %q, want ordinary first-cycle checkpoint", stdout)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Escalated || state.Cycles != 1 {
		t.Fatalf("checkpoint state = %+v, ok = %v, want non-escalated cycle 1", state, ok)
	}
}

func TestRemediationCheckpointIgnoresStaleStructuralConflictEvidenceWithPriorCheckpoint(t *testing.T) {
	baseSHA, attemptedHeadSHA, mergeSHA := initStructuralCollisionCheckpointRepo(t, "goobers/impl/remediation-364")
	runGitT(t, ".", "checkout", "-B", "goobers/impl/remediation-364", "origin/goobers/impl/remediation-364")
	attemptedDigest, err := diffDigest(".", baseSHA)
	if err != nil {
		t.Fatalf("diffDigest attempted head: %v", err)
	}
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: attemptedDigest, HeadSHA: attemptedHeadSHA, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}

	origin := strings.TrimSpace(runGitOutputT(t, ".", "remote", "get-url", "origin"))
	concurrent := filepath.Join(t.TempDir(), "concurrent")
	runGitT(t, ".", "clone", "--branch", "goobers/impl/remediation-364", origin, concurrent)
	runGitT(t, concurrent, "config", "user.name", "human")
	runGitT(t, concurrent, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(concurrent, "concurrent.txt"), []byte("new head\n"), 0o644); err != nil {
		t.Fatalf("write concurrent change: %v", err)
	}
	runGitT(t, concurrent, "add", "concurrent.txt")
	runGitT(t, concurrent, "commit", "-m", "concurrent PR update")
	runGitT(t, concurrent, "push", "origin", "HEAD")
	currentHeadSHA := strings.TrimSpace(runGitOutputT(t, concurrent, "rev-parse", "HEAD"))

	st := &remediationCheckpointServerState{
		number: 77, headSHA: currentHeadSHA, listedHeadSHA: attemptedHeadSHA, baseSHA: baseSHA,
		liveBaseSHA: mergeSHA, labels: []string{needsRemediationLabel},
		comments: []string{priorComment},
		files: []providers.ChangedFile{{
			Path: "status.go", Status: "modified", Patch: structuralCollisionCurrentPatch,
		}},
		siblings: []remediationCheckpointSibling{{
			number: 609, state: "closed", merged: true, updatedAt: time.Now().UTC(),
			mergeSHA: mergeSHA,
			files: []providers.ChangedFile{{
				Path: "status.go", Status: "modified", Patch: structuralCollisionSiblingPatch,
			}},
		}},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	setStructuralCollisionInputs(t, attemptedHeadSHA, mergeSHA)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "recorded checkpoint") {
		t.Fatalf("stdout = %q, want ordinary checkpoint for a new PR head", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Escalated || state.Cycles != 2 || state.HeadSHA != currentHeadSHA {
		t.Fatalf("checkpoint state = %+v, ok = %v, want current head recorded as ordinary cycle 2", state, ok)
	}
	if state.LastDiffDigest == attemptedDigest {
		t.Fatalf("checkpoint digest = %q, want refreshed concurrent-head digest rather than prior attempted-head digest", state.LastDiffDigest)
	}
}

func TestRemediationCheckpointRecomputesDigestWhenHeadChangesBeforePublication(t *testing.T) {
	const branch = "goobers/impl/remediation-364"
	baseSHA, initialHeadSHA := initRemediationCheckpointRepo(t, branch)
	runGitT(t, ".", "checkout", "-B", branch, "origin/"+branch)
	initialDigest, err := diffDigest(".", baseSHA)
	if err != nil {
		t.Fatalf("diffDigest initial head: %v", err)
	}
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: initialDigest, HeadSHA: initialHeadSHA, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}

	origin := strings.TrimSpace(runGitOutputT(t, ".", "remote", "get-url", "origin"))
	concurrent := filepath.Join(t.TempDir(), "concurrent")
	runGitT(t, ".", "clone", "--branch", branch, origin, concurrent)
	runGitT(t, concurrent, "config", "user.name", "human")
	runGitT(t, concurrent, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(concurrent, "concurrent.txt"), []byte("new head\n"), 0o644); err != nil {
		t.Fatalf("write concurrent change: %v", err)
	}
	runGitT(t, concurrent, "add", "concurrent.txt")
	runGitT(t, concurrent, "commit", "-m", "concurrent PR update")
	runGitT(t, concurrent, "push", "origin", "HEAD")
	advancedHeadSHA := strings.TrimSpace(runGitOutputT(t, concurrent, "rev-parse", "HEAD"))
	advancedDigest, err := diffDigest(concurrent, baseSHA)
	if err != nil {
		t.Fatalf("diffDigest advanced head: %v", err)
	}

	st := &remediationCheckpointServerState{
		number: 77, headSHA: initialHeadSHA, baseSHA: baseSHA,
		labels:            []string{needsRemediationLabel},
		comments:          []string{priorComment},
		headAfterComments: advancedHeadSHA,
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "recorded checkpoint") {
		t.Fatalf("stdout = %q, want ordinary checkpoint for the advanced head", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, concurrent head must not be escalated using the prior head's digest", st.labels)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Escalated || state.Cycles != 2 {
		t.Fatalf("checkpoint state = %+v, ok = %v, want ordinary cycle 2", state, ok)
	}
	if state.HeadSHA != advancedHeadSHA || state.LastDiffDigest != advancedDigest {
		t.Fatalf(
			"checkpoint head/digest = %q/%q, want recomputed %q/%q",
			state.HeadSHA, state.LastDiffDigest, advancedHeadSHA, advancedDigest,
		)
	}
}

func TestRemediationCheckpointRecomputesDigestWhenBaseChangesBeforePublication(t *testing.T) {
	const branch = "goobers/impl/remediation-364"
	initialBaseSHA, headSHA := initRemediationCheckpointRepo(t, branch)
	runGitT(t, ".", "checkout", "-B", branch, "origin/"+branch)
	initialDigest, err := diffDigest(".", initialBaseSHA)
	if err != nil {
		t.Fatalf("diffDigest initial base: %v", err)
	}
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: initialDigest, HeadSHA: headSHA, BaseSHA: initialBaseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}

	origin := strings.TrimSpace(runGitOutputT(t, ".", "remote", "get-url", "origin"))
	concurrent := filepath.Join(t.TempDir(), "concurrent")
	runGitT(t, ".", "clone", origin, concurrent)
	runGitT(t, concurrent, "config", "user.name", "human")
	runGitT(t, concurrent, "config", "user.email", "human@example.com")
	runGitT(t, concurrent, "merge", "--no-ff", "origin/"+branch, "-m", "merge PR into advancing base")
	runGitT(t, concurrent, "push", "origin", "main")
	advancedBaseSHA := strings.TrimSpace(runGitOutputT(t, concurrent, "rev-parse", "HEAD"))
	runGitT(t, concurrent, "checkout", branch)
	advancedDigest, err := diffDigest(concurrent, advancedBaseSHA)
	if err != nil {
		t.Fatalf("diffDigest advanced base: %v", err)
	}
	if advancedDigest == initialDigest {
		t.Fatalf("test setup produced identical digests %q; base race would not be observable", advancedDigest)
	}

	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: initialBaseSHA,
		labels:            []string{needsRemediationLabel},
		comments:          []string{priorComment},
		baseAfterComments: advancedBaseSHA,
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "recorded checkpoint") {
		t.Fatalf("stdout = %q, want ordinary checkpoint for the advanced base", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if hasAnyLabel(st.labels, []string{remediationEscalatedLabel}) {
		t.Fatalf("labels = %v, concurrent base advance must not be escalated using the prior base's digest", st.labels)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.Escalated || state.Cycles != 2 {
		t.Fatalf("checkpoint state = %+v, ok = %v, want ordinary cycle 2", state, ok)
	}
	if state.BaseSHA != advancedBaseSHA || state.LastDiffDigest != advancedDigest {
		t.Fatalf(
			"checkpoint base/digest = %q/%q, want recomputed %q/%q",
			state.BaseSHA, state.LastDiffDigest, advancedBaseSHA, advancedDigest,
		)
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
	assertTerminalCheckpointResult(t, "checkpoint-result.json", 77)
}

func TestRemediationCheckpointClassifiesTerminalPRPastCachedList(t *testing.T) {
	tests := []struct {
		name               string
		merged             bool
		terminalOnComments bool
		escalate           bool
	}{
		{name: "closed before exact read"},
		{name: "merged before escalation publication", merged: true, terminalOnComments: true, escalate: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
			st := &remediationCheckpointServerState{
				number: 77, headSHA: headSHA, baseSHA: baseSHA,
				labels: []string{needsRemediationLabel},
			}
			server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
			instanceRoot := remediationCheckpointEnv(t, server.URL, false)

			const snapshotID = "tick-before-checkpoint"
			t.Setenv(providersnapshot.EnvVar, snapshotID)
			repo, err := providerRepo(instanceRoot)
			if err != nil {
				t.Fatalf("providerRepo: %v", err)
			}
			cached := newCachedGitHubProvider(instanceRoot, "test-token")
			prs, err := cached.ListPullRequests(t.Context(), providers.ListPullRequestsRequest{
				Repository: repo, Base: "main", HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
			})
			if err != nil || len(prs) != 1 {
				t.Fatalf("seed pull-request snapshot: prs=%v, err=%v", prs, err)
			}

			st.mu.Lock()
			if tt.terminalOnComments {
				st.terminalOnComments = true
				st.mergeOnComments = tt.merged
			} else {
				st.state = "closed"
				st.merged = tt.merged
			}
			st.mu.Unlock()

			var code int
			var stdout, stderr string
			if tt.escalate {
				code, stdout, stderr = runArgs(t, "remediation-checkpoint", "--escalate", "reviewer rejected", instanceRoot)
			} else {
				code, stdout, stderr = runArgs(t, "remediation-checkpoint", instanceRoot)
			}
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if !strings.Contains(stdout, "no longer open") {
				t.Fatalf("stdout = %q, want terminal-PR business outcome", stdout)
			}
			assertTerminalCheckpointResult(t, "checkpoint-result.json", 77)

			st.mu.Lock()
			defer st.mu.Unlock()
			if st.pullListRequests != 1 {
				t.Fatalf("pull-list requests = %d, want command to replay the seeded provider snapshot", st.pullListRequests)
			}
			if len(st.comments) != 0 {
				t.Fatalf("comments = %v, want terminal PR untouched", st.comments)
			}
			if len(st.labels) != 1 || st.labels[0] != needsRemediationLabel {
				t.Fatalf("labels = %v, want terminal PR labels untouched", st.labels)
			}
		})
	}
}

func TestRemediationCheckpointKeepsUnclassifiedReadErrorGeneric(t *testing.T) {
	initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{
		number: 77, pullReadStatus: http.StatusInternalServerError,
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	// The 500 is here to make the PR read fail unclassifiably, not to time
	// the backoff ladder. Spend the transient-retry budget so the stage
	// fails fast instead of burning 1+2+4+8 = 15s of real sleep;
	// remediationCheckpointEnv's t.Cleanup still restores the factory.
	baseFactory := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return baseFactory(token, append(opts, providers.WithMaxTransientRetries(0))...)
	}

	code, _, _ := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, want provider-stage failure", code)
	}
	result := readProviderStageResult(t, "checkpoint-result.json")
	for _, key := range []string{
		executor.OutputErrorCode,
		executor.OutputErrorMessage,
		executor.OutputErrorRetryable,
	} {
		if _, ok := result[key]; !ok {
			t.Fatalf("result = %v, want generic provider field %q", result, key)
		}
	}
	for _, key := range []string{"continueRemediation", "selectedNumber", "head", "headSha"} {
		if _, ok := result[key]; ok {
			t.Fatalf("result = %v, unclassified provider error must not masquerade as a checkpoint outcome", result)
		}
	}
}

func TestRemediationCheckpointRecreatesConcurrentlyDeletedStickyComment(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles: 1, HeadSHA: headSHA, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:              []string{needsRemediationLabel},
		comments:            []string{priorComment},
		deleteCommentOnEdit: true,
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	code, stdout, stderr := runArgs(
		t,
		"remediation-checkpoint",
		"--escalate", "finding response repass budget exhausted",
		"--escalation-outcome", "budget-exhausted",
		instanceRoot,
	)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readCheckpointResult(t, "checkpoint-result.json")
	want := map[string]string{
		"continueRemediation":  "false",
		"selectedNumber":       "77",
		"head":                 "goobers/impl/remediation-364",
		"headSha":              headSHA,
		"escalationOutcome":    string(remediationOutcomeBudgetExhausted),
		"remediationAttempted": "true",
		"attemptedCauses":      "",
		"escalationReason":     "finding response repass budget exhausted",
		"escalationGeneration": "1",
		"integrity":            string(apiv1.IntegrityUnapproved),
	}
	if len(result) != len(want) {
		t.Fatalf("checkpoint result = %v, want classified result %v", result, want)
	}
	for key, value := range want {
		if result[key] != value {
			t.Fatalf("checkpoint result[%q] = %q, want %q (result: %v)", key, result[key], value, result)
		}
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 1 || !strings.Contains(st.comments[0], "finding response repass budget exhausted") {
		t.Fatalf("comments = %v, want deleted sticky comment recreated with budget escalation state", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || state.EscalationOutcome != remediationOutcomeBudgetExhausted || !state.RemediationAttempted {
		t.Fatalf("recreated escalation state = %+v, ok = %v", state, ok)
	}
}

func assertTerminalCheckpointResult(t *testing.T, path string, selectedNumber int) {
	t.Helper()
	result := readCheckpointResult(t, path)
	want := map[string]string{
		"continueRemediation":  "false",
		"selectedNumber":       strconv.Itoa(selectedNumber),
		"head":                 "",
		"headSha":              "",
		"escalationOutcome":    "",
		"remediationAttempted": "false",
		"attemptedCauses":      "",
		"escalationReason":     "",
		"escalationGeneration": "0",
		"integrity":            string(apiv1.IntegrityUnapproved),
	}
	if len(result) != len(want) {
		t.Fatalf("checkpoint result = %v, want complete terminal result %v", result, want)
	}
	for key, value := range want {
		if result[key] != value {
			t.Fatalf("checkpoint result[%q] = %q, want %q (result: %v)", key, result[key], value, result)
		}
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

// TestRemediationCheckpointResetsBudgetWhenOperatorClearsEscalation is #1808's
// acceptance: the escalation comment promises "a human removes
// goobers:merge-escalated" as an unpark path, and it did not work. The repass
// count lives in this checkpoint's comment payload rather than the label, so
// clearing the label re-admitted the PR with its counter still over budget and
// the next cycle re-escalated immediately — on PR #1729, in under six minutes,
// with the count stuck at 12/10 the whole time.
//
// Prior state here is an escalated, over-budget record; the PR no longer
// carries the label. One cycle must run without re-escalating.
func TestRemediationCheckpointResetsBudgetWhenOperatorClearsEscalation(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles:           12,
		AttemptsByCause:  remediationAttempts{Substantive: 10},
		Escalated:        true,
		EscalatedReason:  "substantive budget exhausted (10/10 attempts)",
		EscalatedHeadSHA: headSHA,
		EscalatedBaseSHA: baseSHA,
		HeadSHA:          headSHA,
		BaseSHA:          baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		// The operator has removed goobers:merge-escalated.
		labels:   []string{needsRemediationLabel},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--budget", "10", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.labels {
		if l == remediationEscalatedLabel {
			t.Fatalf("PR was re-escalated on the very next cycle after an operator cleared the "+
				"escalation — the documented unpark path is inert (labels = %v, stdout = %q)",
				st.labels, stdout)
		}
	}
}

// TestRemediationCheckpointKeepsBudgetWhileStillEscalated is the over-reach
// guard: the reset must key on the operator having cleared the label, not on
// the record merely being an escalation. A PR still carrying
// goobers:merge-escalated must keep its counter, or escalation would never
// stick and the budget would mean nothing.
func TestRemediationCheckpointKeepsBudgetWhileStillEscalated(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	priorComment, err := remediationStateComment(remediationState{
		Cycles:           12,
		AttemptsByCause:  remediationAttempts{Substantive: 10},
		Escalated:        true,
		EscalatedReason:  "substantive budget exhausted (10/10 attempts)",
		EscalatedHeadSHA: headSHA,
		EscalatedBaseSHA: baseSHA,
		HeadSHA:          headSHA,
		BaseSHA:          baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels:   []string{needsRemediationLabel, remediationEscalatedLabel},
		comments: []string{priorComment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--budget", "10", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "escalated") {
		t.Fatalf("stdout = %q, want the over-budget PR to stay escalated", stdout)
	}
}

// TestEscalationParkUnparkMatrix is the sticky-parking acceptance: a park is
// released by a base-branch advance ONLY when the recorded escalation cause is
// one a rebase can plausibly cure, while a head change always releases it.
// Before this, ANY base advance unparked ANY escalation, so in a repo merging
// dozens of PRs a day a deterministically-doomed PR re-entered remediation
// within minutes and burned an agent session each time.
func TestEscalationParkUnparkMatrix(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	const (
		parkedHead = "head-at-escalation"
		parkedBase = "base-at-escalation"
	)

	causeClasses := []struct {
		name                string
		state               remediationState
		baseAdvanceUnblocks bool
		legacyRecord        bool
	}{
		{
			name: "conflict cause: a rebase onto the new base can cure it",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
				EscalationCauses:  []remediationCause{remediationCauseConflict},
			},
			baseAdvanceUnblocks: true,
		},
		{
			name: "sibling-overlap cause: the sibling landing IS the base advance",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
				EscalationCauses:  []remediationCause{remediationCauseSiblingOverlap},
			},
			baseAdvanceUnblocks: true,
		},
		{
			name: "substantive cause: the PR's own content is what was rejected",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
				EscalationCauses:  []remediationCause{remediationCauseSubstantive},
			},
		},
		{
			// #4058: CI is evaluated against merge(base, head), so a base
			// advance re-decides it — and a park the BASE caused (a broken
			// harness on main, later fixed) is cured by exactly that.
			name: "failing-ci cause: CI runs against merge(base, head), so the base tip re-decides it",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
				EscalationCauses:  []remediationCause{remediationCauseFailingCI},
			},
			baseAdvanceUnblocks: true,
		},
		{
			name: "human-comment cause: only a human or a new commit resolves it",
			state: remediationState{
				EscalationOutcome: remediationOutcomeDidNotConverge,
				EscalationCauses:  []remediationCause{remediationCauseHumanComment},
			},
		},
		{
			name: "mixed causes: one non-curable cause keeps the whole park",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
				EscalationCauses:  []remediationCause{remediationCauseConflict, remediationCauseSubstantive},
			},
		},
		{
			name: "mixed causes: every curable cause releases the park",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
				EscalationCauses:  []remediationCause{remediationCauseConflict, remediationCauseFailingCI},
			},
			baseAdvanceUnblocks: true,
		},
		{
			name: "infrastructure failure: the PR was never judged on its merits",
			state: remediationState{
				EscalationOutcome: remediationOutcomeInfrastructure,
			},
		},
		{
			name: "policy exclusion: no declared policy admits this cause",
			state: remediationState{
				EscalationOutcome: remediationOutcomePolicyExcluded,
			},
		},
		{
			name: "forced escalation (reviewer fail): no remediation cause was observed",
			state: remediationState{
				EscalationOutcome: remediationOutcomeDidNotConverge,
			},
		},
		{
			name: "structural collision: retrying the patch cannot resolve it",
			state: remediationState{
				EscalationOutcome:          remediationOutcomeDidNotConverge,
				EscalationCauses:           []remediationCause{remediationCauseConflict},
				StructuralCollisionContext: "- `runStatus` in `status.go` was restructured by merged PR #900",
			},
		},
		{
			// Back-compat: a record written before the cause class was
			// persisted (EscalationGeneration zero) keeps the original
			// unconditional base-advance self-heal rather than being
			// retro-parked on a cause nobody can read.
			name: "escalation recorded before the cause class shipped",
			state: remediationState{
				EscalationOutcome: remediationOutcomeBudgetExhausted,
			},
			baseAdvanceUnblocks: true,
			legacyRecord:        true,
		},
	}

	movements := []struct {
		name        string
		prHeadSHA   string
		liveBaseTip string
		wantBlocked func(baseAdvanceUnblocks bool) bool
	}{
		{
			name: "nothing moved", prHeadSHA: parkedHead, liveBaseTip: parkedBase,
			wantBlocked: func(bool) bool { return true },
		},
		{
			name: "base advanced", prHeadSHA: parkedHead, liveBaseTip: "base-after-sibling-merge",
			wantBlocked: func(baseAdvanceUnblocks bool) bool { return !baseAdvanceUnblocks },
		},
		{
			name: "head moved", prHeadSHA: "head-after-new-commit", liveBaseTip: parkedBase,
			wantBlocked: func(bool) bool { return false },
		},
	}

	for _, class := range causeClasses {
		for _, move := range movements {
			t.Run(class.name+"/"+move.name, func(t *testing.T) {
				state := class.state
				state.Escalated = true
				state.EscalatedReason = "recorded escalation"
				state.EscalatedHeadSHA = parkedHead
				state.EscalatedBaseSHA = parkedBase
				if !class.legacyRecord {
					// Every escalation this binary records carries a
					// generation; a zero one marks the legacy record above.
					state.EscalationGeneration = 1
				}
				comment, err := remediationStateComment(state)
				if err != nil {
					t.Fatalf("remediationStateComment: %v", err)
				}
				server := newFakeGitHubServer(t, repo.Owner, repo.Name)
				server.addIssue(1, "parked pr")
				server.addComment(1, comment)
				server.setBranchTip("main", move.liveBaseTip)
				pr := providers.PullRequestSummary{
					Number: 1, Base: "main", HeadSHA: move.prHeadSHA, BaseSHA: parkedBase,
					Labels: []string{remediationEscalatedLabel},
				}

				blocked, err := escalationStillBlocks(context.Background(), server.newGitHubProvider("token"), repo, pr)
				if err != nil {
					t.Fatalf("escalationStillBlocks: %v", err)
				}
				want := move.wantBlocked(class.baseAdvanceUnblocks)
				if blocked != want {
					t.Fatalf("blocked = %t, want %t", blocked, want)
				}
			})
		}
	}
}

// TestNextEscalationGeneration covers the churn counter's edges directly: a
// fresh park, a re-park of the same head, a park of a moved head, and an
// escalation record written before the counter shipped (which counts as that
// head's first park rather than restarting at 1).
func TestNextEscalationGeneration(t *testing.T) {
	tests := []struct {
		name    string
		prior   remediationState
		headSHA string
		want    int
	}{
		{name: "no prior state", headSHA: "h1", want: 1},
		{
			name:    "prior cycle was not an escalation",
			prior:   remediationState{HeadSHA: "h1", BaseSHA: "b1"},
			headSHA: "h1",
			want:    1,
		},
		{
			name:    "re-escalating the same head",
			prior:   remediationState{Escalated: true, EscalatedHeadSHA: "h1", EscalationGeneration: 7},
			headSHA: "h1",
			want:    8,
		},
		{
			name:    "escalating a moved head restarts the count",
			prior:   remediationState{Escalated: true, EscalatedHeadSHA: "h1", EscalationGeneration: 7},
			headSHA: "h2",
			want:    1,
		},
		{
			name:    "escalation predating the counter counts as the first park",
			prior:   remediationState{Escalated: true, EscalatedHeadSHA: "h1"},
			headSHA: "h1",
			want:    2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextEscalationGeneration(tt.prior, tt.headSHA); got != tt.want {
				t.Fatalf("nextEscalationGeneration = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestRemediationCheckpointRecordsCauseClassAndGeneration is the writer-side
// half of the sticky park: the escalation records the cause class the reader
// keys off, counts this head's parks, and publishes the generation in the
// stage result so a workflow or telemetry query can see the churn.
func TestRemediationCheckpointRecordsCauseClassAndGeneration(t *testing.T) {
	tests := []struct {
		name              string
		cause             string
		priorGeneration   int
		priorHeadIsLive   bool
		wantGeneration    int
		wantCauses        []remediationCause
		wantBaseCanUnpark bool
	}{
		{
			// #4058: CI runs against merge(base, head), so a base advance is
			// new evidence about a failing-ci park — including one the base
			// itself caused.
			name:  "first park of a failing-ci PR is generation 1 and stays rebase-curable",
			cause: "failing-ci", wantGeneration: 1,
			wantCauses: []remediationCause{remediationCauseFailingCI}, wantBaseCanUnpark: true,
		},
		{
			name:  "re-escalating the same head counts up",
			cause: "failing-ci", priorGeneration: 3, priorHeadIsLive: true, wantGeneration: 4,
			wantCauses: []remediationCause{remediationCauseFailingCI}, wantBaseCanUnpark: true,
		},
		{
			name:  "a conflict park stays rebase-curable",
			cause: "conflict", wantGeneration: 1,
			wantCauses: []remediationCause{remediationCauseConflict}, wantBaseCanUnpark: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
			priorHead := "head-before-the-last-push"
			if tt.priorHeadIsLive {
				priorHead = headSHA
			}
			priorComment, err := remediationStateComment(remediationState{
				Cycles: 2, AttemptsByCause: remediationAttempts{Conflict: 2, FailingCI: 2},
				LastDiffDigest:       "sha256:stale-digest-from-a-different-diff",
				Escalated:            true,
				EscalatedReason:      "a prior park",
				EscalatedHeadSHA:     priorHead,
				EscalatedBaseSHA:     baseSHA,
				EscalationGeneration: tt.priorGeneration,
			})
			if err != nil {
				t.Fatalf("remediationStateComment: %v", err)
			}
			st := &remediationCheckpointServerState{
				number: 77, headSHA: headSHA, baseSHA: baseSHA,
				// Still labelled: an operator clearing the label is its own
				// reset path, which must not fire here.
				labels:   []string{needsRemediationLabel, remediationEscalatedLabel},
				comments: []string{priorComment},
			}
			server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

			instanceRoot := remediationCheckpointEnv(t, server.URL, false)
			t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", tt.cause)
			code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}

			st.mu.Lock()
			defer st.mu.Unlock()
			state, ok := parseRemediationStateComment(st.comments[0])
			if !ok || !state.Escalated {
				t.Fatalf("sticky comment %q -> state=%+v ok=%v, want a recorded escalation", st.comments[0], state, ok)
			}
			if state.EscalationGeneration != tt.wantGeneration {
				t.Fatalf("escalation generation = %d, want %d", state.EscalationGeneration, tt.wantGeneration)
			}
			if !reflect.DeepEqual(state.EscalationCauses, tt.wantCauses) {
				t.Fatalf("escalation causes = %v, want %v", state.EscalationCauses, tt.wantCauses)
			}
			if got := escalationBaseAdvanceUnparks(state); got != tt.wantBaseCanUnpark {
				t.Fatalf("base advance unparks = %t, want %t for cause %q", got, tt.wantBaseCanUnpark, tt.cause)
			}
			result := readCheckpointResult(t, "checkpoint-result.json")
			if result["escalationGeneration"] != strconv.Itoa(tt.wantGeneration) {
				t.Fatalf("checkpoint result escalationGeneration = %q, want %q (result: %v)",
					result["escalationGeneration"], strconv.Itoa(tt.wantGeneration), result)
			}
			if !strings.Contains(st.comments[0], "Parked until") {
				t.Fatalf("sticky comment = %q, want the parked-until prose", st.comments[0])
			}
			wantProse := "Parked until this PR's head changes"
			if tt.wantBaseCanUnpark {
				wantProse = "Parked until this PR's head or base changes"
			}
			if !strings.Contains(st.comments[0], wantProse) {
				t.Fatalf("sticky comment = %q, want it to advertise %q", st.comments[0], wantProse)
			}
			if want := fmt.Sprintf("escalation %d for head", tt.wantGeneration); !strings.Contains(stdout, want) {
				t.Fatalf("stdout = %q, want the churn count %q", stdout, want)
			}
		})
	}
}

// TestRefreshEscalationSnapshotAfterRepeatFailHardensPark: merge-review
// re-reviewing a parked PR and failing it again is evidence the last unpark was
// wasted, so the refreshed snapshot counts the park and drops the rebase-curable
// cause — a further base advance is not new information about content a reviewer
// just re-rejected at this same head.
func TestRefreshEscalationSnapshotAfterRepeatFailHardensPark(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	comment, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedReason:      "conflict budget exhausted",
		EscalatedHeadSHA:     "h1",
		EscalatedBaseSHA:     "base-at-escalation",
		EscalationCauses:     []remediationCause{remediationCauseConflict},
		EscalationGeneration: 1,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(1, "re-failed pr")
	server.addComment(1, comment)
	server.setBranchTip("main", "base-after-sibling-merge")
	provider := server.newGitHubProvider("token")
	pr := providers.PullRequestSummary{
		Number: 1, Base: "main", HeadSHA: "h1", BaseSHA: "base-at-escalation",
		Labels: []string{remediationEscalatedLabel},
	}

	comments, err := provider.ListComments(context.Background(), repo, "1")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if err := refreshEscalationSnapshotAfterRepeatFail(context.Background(), provider, repo, pr, comments); err != nil {
		t.Fatalf("refreshEscalationSnapshotAfterRepeatFail: %v", err)
	}

	refreshed, err := provider.ListComments(context.Background(), repo, "1")
	if err != nil {
		t.Fatalf("ListComments after refresh: %v", err)
	}
	state, _, found := latestRemediationState(refreshed)
	if !found {
		t.Fatalf("comments = %v, want the sticky state edited in place", refreshed)
	}
	if state.EscalationGeneration != 2 {
		t.Fatalf("escalation generation = %d, want 2 (the same head parked again)", state.EscalationGeneration)
	}
	if len(state.EscalationCauses) != 0 {
		t.Fatalf("escalation causes = %v, want none after a reviewer re-fail", state.EscalationCauses)
	}
	if escalationBaseAdvanceUnparks(state) {
		t.Fatal("base advance still unparks a PR merge-review just re-failed at the same head")
	}

	// The next base advance must leave the PR parked; only a new head (or a
	// human removing the label) may release it.
	server.setBranchTip("main", "base-after-another-sibling-merge")
	blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
	if err != nil {
		t.Fatalf("escalationStillBlocks: %v", err)
	}
	if !blocked {
		t.Fatal("blocked = false, want true — a further base advance must not unpark a re-failed PR")
	}
	pr.HeadSHA = "h2"
	blocked, err = escalationStillBlocks(context.Background(), provider, repo, pr)
	if err != nil {
		t.Fatalf("escalationStillBlocks: %v", err)
	}
	if blocked {
		t.Fatal("blocked = true, want false — pushing a commit must always remain an escape hatch")
	}
}
