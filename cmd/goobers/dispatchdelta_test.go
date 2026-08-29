package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
)

// fakeBlobPlane is the smallest thing that behaves like the blob plane: a
// digest-keyed store over HTTP. It deliberately does NOT verify that content
// hashes to its key, matching the real plane, so the verification this code
// does on arrival is exercised rather than assumed.
func fakeBlobPlane(t *testing.T) (string, func(digest string) []byte) {
	t.Helper()
	var mu sync.Mutex
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if i := strings.LastIndex(key, "/"); i >= 0 {
			key = key[i+1:]
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body := make([]byte, 0)
			buf := make([]byte, 4096)
			for {
				n, err := r.Body.Read(buf)
				body = append(body, buf[:n]...)
				if err != nil {
					break
				}
			}
			store["sha256:"+key] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			data, ok := store["sha256:"+key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func(digest string) []byte {
		mu.Lock()
		defer mu.Unlock()
		return store[digest]
	}
}

// The whole point of #3763, end to end through the carrier: a commit made in
// one workspace must be present in a DIFFERENT one that only ever cloned base.
// The two directories stand in for two pods — separate filesystems, no shared
// object store, which is exactly what makes the worker's branch-ref continuity
// unavailable here.
func TestWorkspaceDeltaCarriesACommitBetweenTwoWorkspaces(t *testing.T) {
	origin := initBareOrigin(t)
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

	// --- pod A: clone, commit, publish -----------------------------------
	podA := filepath.Join(t.TempDir(), "a")
	runGitT(t, filepath.Dir(podA), "clone", "--branch", "main", origin, podA)
	runGitT(t, podA, "checkout", "-b", "e2e/wf/run-3763")
	runGitT(t, podA, "config", "user.name", "a")
	runGitT(t, podA, "config", "user.email", "a@example.com")
	if err := os.WriteFile(filepath.Join(podA, "carried.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitT(t, podA, "add", "carried.txt")
	runGitT(t, podA, "commit", "-m", "the commit that must survive")
	want := strings.TrimSpace(runGitOutputT(t, podA, "rev-parse", "HEAD"))

	digest, err := publishWorkspaceDelta(context.Background(), podA, os.Stderr)
	if err != nil {
		t.Fatalf("publishWorkspaceDelta: %v", err)
	}
	if digest == "" {
		t.Fatal("no delta published for a branch that carries a commit — this is the bug, not the fix")
	}

	// The publisher must leave no trace in the workspace: a stray bundle or a
	// leftover ref would show up as drift to whatever runs next.
	if _, err := os.Stat(filepath.Join(podA, "workspace.bundle")); !os.IsNotExist(err) {
		t.Fatal("bundle was left inside the workspace")
	}
	if out := strings.TrimSpace(runGitOutputT(t, podA, "for-each-ref", "--format=%(refname)", workspaceDeltaRef)); out != "" {
		t.Fatalf("delta ref left behind in the source repo: %q", out)
	}

	// --- pod B: fresh clone of BASE ONLY, then apply ----------------------
	podB := filepath.Join(t.TempDir(), "b")
	runGitT(t, filepath.Dir(podB), "clone", "--branch", "main", origin, podB)
	runGitT(t, podB, "checkout", "-b", "e2e/wf/run-3763")
	if _, err := os.Stat(filepath.Join(podB, "carried.txt")); !os.IsNotExist(err) {
		t.Fatal("pod B saw the file before applying the delta; the test proves nothing")
	}

	if err := applyWorkspaceDelta(context.Background(), podB, digest, nil, os.Stderr); err != nil {
		t.Fatalf("applyWorkspaceDelta: %v", err)
	}
	if got := strings.TrimSpace(runGitOutputT(t, podB, "rev-parse", "HEAD")); got != want {
		t.Fatalf("pod B HEAD = %s, want the commit pod A made (%s)", got, want)
	}
	if _, err := os.Stat(filepath.Join(podB, "carried.txt")); err != nil {
		t.Fatalf("the committed file did not arrive in pod B: %v", err)
	}
}

// A repo-readonly stage is detached at base and must never produce a delta:
// carrying one would let a read-only stage silently rewrite what later stages
// see, which is a worse failure than the one being fixed.
func TestWorkspaceDeltaRefusesNonWritableWorkspaces(t *testing.T) {
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	for _, mode := range []apiv1.WorkspaceMode{apiv1.WorkspaceScratch, apiv1.WorkspaceRepoReadOnly, ""} {
		t.Run(string(mode)+"/none", func(t *testing.T) {
			t.Setenv(dispatcher.EnvStageWorkspace, string(mode))
			digest, err := publishWorkspaceDelta(context.Background(), t.TempDir(), os.Stderr)
			if err != nil {
				t.Fatalf("publishWorkspaceDelta(%q) = error %v; a non-writable workspace is ordinary, not a failure", mode, err)
			}
			if digest != "" {
				t.Fatalf("publishWorkspaceDelta(%q) = %q, want no delta", mode, digest)
			}
		})
	}
}

// A substituted delta means running this stage on top of commits nobody in this
// run made. The plane does not verify content addressing, so the receiver must.
func TestWorkspaceDeltaRefusesASubstitutedBundle(t *testing.T) {
	origin := initBareOrigin(t)
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")

	// Publish bytes under a digest that is NOT their content address.
	client := &dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}
	lie := agentickit.Digest([]byte("some other content entirely"))
	if err := client.Put(context.Background(), lie, []byte("not a bundle")); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	podB := filepath.Join(t.TempDir(), "b")
	runGitT(t, filepath.Dir(podB), "clone", "--branch", "main", origin, podB)

	err := applyWorkspaceDelta(context.Background(), podB, lie, nil, os.Stderr)
	if err == nil {
		t.Fatal("a delta whose bytes do not hash to its digest was accepted")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want a digest mismatch refusal", err)
	}
}

// publishDeltaFrom is a small helper for the ancestry-guard tests below: it
// commits one file in dir on the given branch (creating the branch off
// whatever is already checked out) and publishes a workspace delta for it,
// returning the digest and the resulting HEAD.
func publishDeltaFrom(t *testing.T, dir, branch, file, content string) (digest, head string) {
	t.Helper()
	runGitT(t, dir, "checkout", "-B", branch)
	// Set per-repo, not relied on from the environment: a clone carries no
	// identity of its own, and these tests must not depend on the host or CI
	// container having a global git user configured.
	runGitT(t, dir, "config", "user.name", "delta-test")
	runGitT(t, dir, "config", "user.email", "delta-test@example.com")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	runGitT(t, dir, "add", file)
	runGitT(t, dir, "commit", "-m", "commit "+file)
	head = strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))
	digest, err := publishWorkspaceDelta(context.Background(), dir, os.Stderr)
	if err != nil {
		t.Fatalf("publishWorkspaceDelta: %v", err)
	}
	if digest == "" {
		t.Fatal("no delta published for a branch that carries a commit")
	}
	return digest, head
}

// The ancestry guard's three arms (#3803/#3767 PR-B): a STALE digest must
// never rewind a checkout that has already moved past it — self-placed
// (worker) stages and provider-side producers such as update-behind-pr
// advance the run branch without ever publishing a delta, so the next pod can
// be handed an older digest than what it already has checked out.
//
// All three tests share one shape: two independent clones of the same run
// branch (standing in for two pods, or a pod after a worker/provider-side
// advance), one publishes a delta, and applyWorkspaceDelta is asked to apply
// it to the OTHER clone, which is independently seeded to be ahead, equal, or
// diverged.
func TestApplyWorkspaceDeltaAncestryGuard(t *testing.T) {
	const branch = "e2e/wf/run-ff-guard"

	t.Run("fast-forward: checkout at base, delta ahead -> applies", func(t *testing.T) {
		origin := initBareOrigin(t)
		endpoint, _ := fakeBlobPlane(t)
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

		publisher := filepath.Join(t.TempDir(), "publisher")
		runGitT(t, filepath.Dir(publisher), "clone", "--branch", "main", origin, publisher)
		digest, head := publishDeltaFrom(t, publisher, branch, "carried.txt", "work\n")

		checkout := filepath.Join(t.TempDir(), "checkout")
		runGitT(t, filepath.Dir(checkout), "clone", "--branch", "main", origin, checkout)
		runGitT(t, checkout, "checkout", "-b", branch)

		if err := applyWorkspaceDelta(context.Background(), checkout, digest, nil, os.Stderr); err != nil {
			t.Fatalf("applyWorkspaceDelta: %v", err)
		}
		if got := strings.TrimSpace(runGitOutputT(t, checkout, "rev-parse", "HEAD")); got != head {
			t.Fatalf("checkout HEAD = %s, want the fast-forwarded delta tip %s", got, head)
		}
	})

	t.Run("behind: checkout already ahead of a stale digest -> no-op, keeps checkout", func(t *testing.T) {
		origin := initBareOrigin(t)
		endpoint, _ := fakeBlobPlane(t)
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

		// The "stale digest": published from a clone that only ever gets one
		// commit, standing in for a pod that ran early in the walk.
		publisher := filepath.Join(t.TempDir(), "publisher")
		runGitT(t, filepath.Dir(publisher), "clone", "--branch", "main", origin, publisher)
		staleDigest, staleHead := publishDeltaFrom(t, publisher, branch, "first.txt", "first\n")

		// The checkout under test: built ON TOP of that same commit and then
		// advanced FURTHER — standing in for a self-placed (worker) stage or a
		// provider-side producer (update-behind-pr) that moved the branch past
		// this digest without ever publishing one, exactly the gap this guard
		// exists to cover. Fetched directly from the publisher's own branch
		// ref (not through applyWorkspaceDelta) so this setup step does not
		// depend on the function under test.
		checkout := filepath.Join(t.TempDir(), "checkout")
		runGitT(t, filepath.Dir(checkout), "clone", "--branch", "main", origin, checkout)
		runGitT(t, checkout, "checkout", "-b", branch)
		runGitT(t, checkout, "fetch", publisher, branch)
		runGitT(t, checkout, "reset", "--hard", "FETCH_HEAD")
		if got := strings.TrimSpace(runGitOutputT(t, checkout, "rev-parse", "HEAD")); got != staleHead {
			t.Fatalf("test setup: checkout HEAD = %s, want it seeded at the stale digest's tip %s", got, staleHead)
		}
		_, aheadHead := publishDeltaFrom(t, checkout, branch, "second.txt", "second\n")
		if aheadHead == staleHead {
			t.Fatal("test setup did not actually advance the checkout past the stale digest")
		}
		beforeHead := strings.TrimSpace(runGitOutputT(t, checkout, "rev-parse", "HEAD"))

		var captured strings.Builder
		if err := applyWorkspaceDelta(context.Background(), checkout, staleDigest, nil, &captured); err != nil {
			t.Fatalf("applyWorkspaceDelta returned an error for a stale-but-ancestor digest: %v", err)
		}
		if got := strings.TrimSpace(runGitOutputT(t, checkout, "rev-parse", "HEAD")); got != beforeHead {
			t.Fatalf("checkout HEAD moved from %s to %s; a stale digest must never rewind the checkout", beforeHead, got)
		}
		msg := captured.String()
		if !strings.Contains(msg, "workspace delta is behind the checkout") {
			t.Fatalf("stderr = %q, want it to name the behind-checkout no-op", msg)
		}
		if !strings.Contains(msg, beforeHead) || !strings.Contains(msg, staleHead) {
			t.Fatalf("stderr = %q, want it to name both the kept checkout SHA (%s) and the stale delta's SHA (%s)", msg, beforeHead, staleHead)
		}
	})

	t.Run("diverged: neither is an ancestor of the other -> fails closed, checkout untouched", func(t *testing.T) {
		origin := initBareOrigin(t)
		endpoint, _ := fakeBlobPlane(t)
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

		// Both sides start from the SAME commit (a shared ancestor beyond base)
		// and then commit independently, so neither is reachable from the
		// other — a genuine divergence, not merely a stale digest.
		shared := filepath.Join(t.TempDir(), "shared")
		runGitT(t, filepath.Dir(shared), "clone", "--branch", "main", origin, shared)
		_, sharedHead := publishDeltaFrom(t, shared, branch, "shared.txt", "shared\n")
		runGitT(t, shared, "push", origin, branch+":"+branch)

		branchDigest := filepath.Join(t.TempDir(), "branch-a")
		runGitT(t, filepath.Dir(branchDigest), "clone", "--branch", branch, origin, branchDigest)
		divergedDigest, divergedHead := publishDeltaFrom(t, branchDigest, branch, "a.txt", "a\n")
		if divergedHead == sharedHead {
			t.Fatal("test setup did not diverge the publisher from the shared ancestor")
		}

		checkout := filepath.Join(t.TempDir(), "branch-b")
		runGitT(t, filepath.Dir(checkout), "clone", "--branch", branch, origin, checkout)
		runGitT(t, checkout, "config", "user.name", "b")
		runGitT(t, checkout, "config", "user.email", "b@example.com")
		if err := os.WriteFile(filepath.Join(checkout, "b.txt"), []byte("b\n"), 0o644); err != nil {
			t.Fatalf("write b.txt: %v", err)
		}
		runGitT(t, checkout, "add", "b.txt")
		runGitT(t, checkout, "commit", "-m", "diverge from a")
		beforeHead := strings.TrimSpace(runGitOutputT(t, checkout, "rev-parse", "HEAD"))
		if beforeHead == divergedHead {
			t.Fatal("test setup did not diverge the checkout from the delta")
		}

		err := applyWorkspaceDelta(context.Background(), checkout, divergedDigest, nil, os.Stderr)
		if err == nil {
			t.Fatal("applyWorkspaceDelta accepted a diverged digest instead of failing closed")
		}
		if !strings.Contains(err.Error(), "diverged") {
			t.Fatalf("error = %v, want it to name the divergence", err)
		}
		if !strings.Contains(err.Error(), beforeHead) || !strings.Contains(err.Error(), divergedHead) {
			t.Fatalf("error = %v, want it to name both the checkout's SHA (%s) and the delta's SHA (%s)", err, beforeHead, divergedHead)
		}
		if got := strings.TrimSpace(runGitOutputT(t, checkout, "rev-parse", "HEAD")); got != beforeHead {
			t.Fatalf("checkout HEAD moved from %s to %s on a failed apply; a rejected divergence must leave the checkout untouched", beforeHead, got)
		}
	})
}

// A writable repo workspace whose branch cannot be determined must FAIL, not
// report "nothing to carry".
//
// MEASURED, and the reason this test exists: in-pod, every git call in the
// delta path failed with "detected dubious ownership" (exit 128) because
// /workspace is not owned by the container user and only the STAGE's own
// command inherits the safe.directory exemption. publishWorkspaceDelta treated
// the resulting currentBranch failure as an ordinary skip, so four consecutive
// runs reported success while carrying nothing — the exact silent shape #3763
// exists to remove, reintroduced by its own fix.
//
// Any git failure here is indistinguishable from "the stage committed and we
// cannot tell", so the only safe answer is to fail loudly.
func TestWorkspaceDeltaFailsLoudlyWhenTheBranchCannotBeDetermined(t *testing.T) {
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

	// Not a git repository at all: currentBranch cannot succeed, standing in
	// for every reason git might refuse the workspace.
	notARepo := t.TempDir()

	digest, err := publishWorkspaceDelta(context.Background(), notARepo, os.Stderr)
	if err == nil {
		t.Fatalf("publishWorkspaceDelta returned digest %q and no error; a writable repo workspace whose branch is undeterminable must fail rather than silently carry nothing", digest)
	}
	if digest != "" {
		t.Fatalf("publishWorkspaceDelta returned digest %q alongside an error", digest)
	}
	if !strings.Contains(err.Error(), "cannot determine the checked-out branch") {
		t.Fatalf("error = %v, want it to name the undeterminable branch", err)
	}
}
