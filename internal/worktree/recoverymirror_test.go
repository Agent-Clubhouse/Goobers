package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

func TestRecoveryMirrorRefusesInvalidPathsBeforeVisitor(t *testing.T) {
	for _, mode := range []string{"parent-file", "mirror-file", "staging-file", "cancelled", "empty-url"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			url := "https://example.invalid/team/repo.git"
			parent := filepath.Join(m.Root, repoKey(url))
			path := parent
			if mode == "mirror-file" || mode == "staging-file" {
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(parent, "repo.git")
				if mode == "staging-file" {
					path = filepath.Join(parent, "recovery-mirror-init")
				}
			}
			if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			if mode == "empty-url" {
				url = ""
			}
			if err := m.WithRecoveryMirror(ctx, url, func(string) error {
				t.Fatal("invalid mirror visited")
				return nil
			}); err == nil {
				t.Fatal("invalid mirror accepted")
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "preserve" {
				t.Fatalf("invalid path mutated: %q %v", data, err)
			}
		})
	}
}

func TestRecoveryMirrorEnvironmentExcludesInheritedGitOverrides(t *testing.T) {
	t.Setenv("GIT_DIR", "/foreign/repository")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", "/foreign/worktree")
	t.Setenv("GIT_OBJECT_DIRECTORY", "/foreign/objects")
	for _, entry := range recoveryMirrorEnvironment() {
		if strings.HasPrefix(strings.ToUpper(entry), "GIT_") && entry != "GIT_CONFIG_NOSYSTEM=1" && entry != "GIT_CONFIG_GLOBAL="+os.DevNull {
			t.Fatalf("inherited Git override leaked: %q", entry)
		}
	}
}

func TestEnsureRecoveryBaseFetchesMissingBaseOnce(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	recoveryMirrorTestGit(t, source, "init", "--initial-branch=main")
	recoveryMirrorTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	base := recoveryMirrorTestGit(t, source, "rev-parse", "HEAD")
	fetches := 0
	m, err := NewManager(t.TempDir(), WithRemoteGitGate(func(context.Context, string) error {
		fetches++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WithRecoveryMirror(ctx, source, func(mirror string) error {
		if err := rawGitExists(ctx, mirror, base+"^{commit}"); err == nil {
			t.Fatal("new recovery mirror unexpectedly contains source base")
		}
		if err := m.EnsureRecoveryBase(ctx, source, mirror, "refs/heads/main", base); err != nil {
			t.Fatalf("EnsureRecoveryBase: %v", err)
		}
		if err := rawGitExists(ctx, mirror, base+"^{commit}"); err != nil {
			t.Fatalf("base missing after fetch: %v", err)
		}
		if err := m.EnsureRecoveryBase(ctx, source, mirror, "refs/heads/main", base); err != nil {
			t.Fatalf("EnsureRecoveryBase retry: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("remote fetches = %d, want 1", fetches)
	}
}

func recoveryMirrorTestGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(append([]string{"-C", repository}, args...)...)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Recovery Test", "GIT_AUTHOR_EMAIL=recovery@example.invalid", "GIT_COMMITTER_NAME=Recovery Test", "GIT_COMMITTER_EMAIL=recovery@example.invalid")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git failed: %v: %s", err, data)
	}
	return strings.TrimSpace(string(data))
}
