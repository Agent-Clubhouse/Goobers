package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/testgit"
)

// recoveryPodTestGit runs one git command with a deterministic identity, for
// building the local repositories resolveRecoveryBaseRef is tested against.
func recoveryPodTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// #5103: a pod's checkout can leave "base" resolvable only as a local
// refs/heads/<base> branch (a fresh run's fallback clone) or only as
// refs/remotes/origin/<base> (a writable stage cloned directly onto an
// already-existing run branch — see dispatchcheckout.go's
// ensureRecoveryBaseRemoteRef). resolveRecoveryBaseRef must find either, and
// refuse cleanly when neither is present rather than let git's own "unknown
// revision" exit-128 reach the caller unexplained.
func TestResolveRecoveryBaseRef(t *testing.T) {
	t.Run("local branch", func(t *testing.T) {
		dir := t.TempDir()
		recoveryPodTestGit(t, dir, "init", "--quiet", "-b", "main")
		recoveryPodTestGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "base")
		got, err := resolveRecoveryBaseRef(t.Context(), dir, "main")
		if err != nil || got != "refs/heads/main" {
			t.Fatalf("resolveRecoveryBaseRef() = %q, %v, want refs/heads/main", got, err)
		}
	})
	t.Run("remote-tracking ref only", func(t *testing.T) {
		dir := t.TempDir()
		recoveryPodTestGit(t, dir, "init", "--quiet", "-b", "feature")
		recoveryPodTestGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "feature work")
		recoveryPodTestGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		got, err := resolveRecoveryBaseRef(t.Context(), dir, "main")
		if err != nil || got != "refs/remotes/origin/main" {
			t.Fatalf("resolveRecoveryBaseRef() = %q, %v, want refs/remotes/origin/main", got, err)
		}
	})
	t.Run("neither present", func(t *testing.T) {
		dir := t.TempDir()
		recoveryPodTestGit(t, dir, "init", "--quiet", "-b", "feature")
		recoveryPodTestGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "feature work")
		if _, err := resolveRecoveryBaseRef(t.Context(), dir, "main"); err == nil {
			t.Fatal("resolved a base branch this checkout never fetched")
		}
	})
}

func TestPodRecoveryNonWritableWorkspaceNeedsNoCustody(t *testing.T) {
	for _, mode := range []string{"scratch", "repo-readonly"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(dispatcher.EnvStageWorkspace, mode)
			t.Setenv(dispatcher.EnvDaemonAPI, "")
			if err := publishPodRecovery(t.Context(), "missing-workspace"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPodRecoveryMissingClaimPreservesSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/claims/list" || r.Header.Get("Authorization") != "Bearer parent-token" {
			t.Error("recovery did not use the parent claim credential")
		}
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvStageWorkspace, "repo")
	t.Setenv(dispatcher.EnvRunID, "pod-recovery")
	t.Setenv(dispatcher.EnvGaggle, "web")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "parent-token")
	root := t.TempDir()
	path := filepath.Join(root, "implementation")
	if err := os.WriteFile(path, []byte("retain me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishPodRecovery(t.Context(), root); err == nil {
		t.Fatal("missing claim acknowledged recovery")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "retain me" {
		t.Fatalf("unacknowledged source changed: %q %v", data, err)
	}
}
