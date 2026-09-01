package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

// TestMain keeps ordinary fixture directories ordinary even when the suite is
// invoked by a GitHub-hosted runner. Tests that exercise hosted detection set
// RUNNER_ENVIRONMENT explicitly with t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("RUNNER_ENVIRONMENT"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestInspectInitTargetDetectsLinkedWorktree(t *testing.T) {
	repository := t.TempDir()
	runInitSafetyTestGit(t, repository, "init", "-b", "main")
	runInitSafetyTestGit(t, repository, "config", "user.email", "test@example.com")
	runInitSafetyTestGit(t, repository, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInitSafetyTestGit(t, repository, "add", "README.md")
	runInitSafetyTestGit(t, repository, "commit", "-m", "initial")

	linked := filepath.Join(t.TempDir(), "session-worktree")
	runInitSafetyTestGit(t, repository, "worktree", "add", "-b", "session", linked, "main")
	canonicalLinked := canonicalInitPath(linked)

	safety, err := InspectInitTarget(context.Background(), filepath.Join(linked, "instance"))
	if err != nil {
		t.Fatal(err)
	}
	if !safety.Ephemeral || !safety.LinkedWorktree {
		t.Fatalf("safety = %+v, want linked ephemeral target", safety)
	}
	if safety.RepositoryRoot != canonicalLinked || safety.EphemeralRoot != canonicalLinked {
		t.Fatalf("safety roots = %+v, want %q", safety, canonicalLinked)
	}
	if err := CheckInitTarget(context.Background(), filepath.Join(linked, "instance"), false); err == nil ||
		!strings.Contains(err.Error(), "--allow-ephemeral") ||
		!strings.Contains(err.Error(), "goobers/instances") {
		t.Fatalf("CheckInitTarget error = %v, want actionable refusal", err)
	}
	if err := CheckInitTarget(context.Background(), filepath.Join(linked, "instance"), true); err != nil {
		t.Fatalf("explicit override rejected: %v", err)
	}
}

func TestInspectInitTargetDetectsHostedWorkspaceMarker(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("GITHUB_WORKSPACE", workspace)
	canonicalWorkspace := canonicalInitPath(workspace)

	safety, err := InspectInitTarget(context.Background(), filepath.Join(workspace, "instance"))
	if err != nil {
		t.Fatal(err)
	}
	if !safety.Ephemeral || safety.EphemeralRoot != canonicalWorkspace {
		t.Fatalf("safety = %+v, want GitHub workspace marker", safety)
	}
	if !strings.Contains(safety.Reason, "GitHub workspace") {
		t.Fatalf("reason = %q, want GitHub workspace", safety.Reason)
	}
}

func TestInspectInitTargetDetectsGitHubHostedRunnerOutsideWorkspace(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "goobers", "instances", "widget")
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_ENVIRONMENT", "github-hosted")

	safety, err := InspectInitTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !safety.Ephemeral || !safety.HostedSession {
		t.Fatalf("safety = %+v, want GitHub-hosted ephemeral target", safety)
	}
	if !strings.Contains(safety.Reason, "GitHub-hosted runner") {
		t.Fatalf("reason = %q, want GitHub-hosted runner", safety.Reason)
	}
	recommended := RecommendedInstancePath(safety)
	if containedPath(home, recommended) || strings.Contains(recommended, home) {
		t.Fatalf("recommended path = %q, must not use hosted home %q", recommended, home)
	}
	if err := CheckInitTarget(context.Background(), target, false); err == nil ||
		!strings.Contains(err.Error(), "--allow-ephemeral") {
		t.Fatalf("CheckInitTarget error = %v, want explicit override", err)
	}
}

func TestInspectInitTargetDetectsLinkedWorktreeAroundNestedRepository(t *testing.T) {
	repository := t.TempDir()
	runInitSafetyTestGit(t, repository, "init", "-b", "main")
	runInitSafetyTestGit(t, repository, "config", "user.email", "test@example.com")
	runInitSafetyTestGit(t, repository, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInitSafetyTestGit(t, repository, "add", "README.md")
	runInitSafetyTestGit(t, repository, "commit", "-m", "initial")

	linked := filepath.Join(t.TempDir(), "session-worktree")
	runInitSafetyTestGit(t, repository, "worktree", "add", "-b", "session", linked, "main")
	canonicalLinked := canonicalInitPath(linked)
	nested := filepath.Join(linked, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runInitSafetyTestGit(t, nested, "init", "-b", "main")

	safety, err := InspectInitTarget(context.Background(), filepath.Join(nested, "instance"))
	if err != nil {
		t.Fatal(err)
	}
	if !safety.Ephemeral || !safety.LinkedWorktree || safety.RepositoryRoot != canonicalLinked {
		t.Fatalf("safety = %+v, want containing linked worktree %q", safety, canonicalLinked)
	}
}

func TestInspectInitTargetAllowsOrdinaryDirectory(t *testing.T) {
	target := filepath.Join(t.TempDir(), "instance")
	safety, err := InspectInitTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if safety.Ephemeral || safety.LinkedWorktree {
		t.Fatalf("ordinary target classified as unsafe: %+v", safety)
	}
}

func runInitSafetyTestGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := testgit.Command(append([]string{"-C", directory}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
