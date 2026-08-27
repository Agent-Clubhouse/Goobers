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
