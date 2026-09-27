//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationRecoveryCleanupSkipsHarnessOnlyWorktree is #5119's regression.
// A worktree whose ONLY untracked content is the harness's own stage artifacts
// — the stage resultFile and the mutation sidecar — must produce no recovery
// inventory entry at all: once those paths are excluded the captured patch is
// genuinely empty, so the existing empty-patch guard discards it and no
// retention-floored slot is consumed. Before the fix, each such cleanup
// published a one-line bookkeeping archive and an instance refilled its
// inventory within days regardless of the configured cap.
func TestIntegrationRecoveryCleanupSkipsHarnessOnlyWorktree(t *testing.T) {
	testdep.Require(t, "git")
	layout, workspace, cleanup := seedHarnessArtifactWorktree(t)
	defer cleanup()

	// Exactly what the executor does before the stage runs, and what the pod
	// supervisor does before it captures.
	executor.ExcludeStageArtifacts(t.Context(), workspace.Path, "claimed-item.json")
	writeHarnessArtifact(t, workspace.Path, "claimed-item.json", "{\"item\":\"1\"}\n")
	writeHarnessArtifact(t, workspace.Path, "mutations.jsonl", "{\"kind\":\"issue\"}\n")
	// An ignored build output alongside them: excluding the harness artifacts
	// makes the empty-selection case the COMMON one, and an empty selection
	// must not expand into a forced add of the whole worktree (#5362).
	writeHarnessArtifact(t, workspace.Path, "ignored.out", "generated\n")
	if err := os.Mkdir(filepath.Join(workspace.Path, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHarnessArtifact(t, filepath.Join(workspace.Path, "node_modules"), "generated.js", "module.exports = {}\n")

	if status := recoveryCLIGit(t, workspace.Path, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("harness artifacts stayed visible to git: %q", status)
	}
	target := worktree.CleanupTarget{
		Path:     workspace.Path,
		StartRef: recoveryCLIGit(t, workspace.Path, "rev-parse", "HEAD"),
	}
	if err := worktree.VerifyCleanupTargetUnchanged(t.Context(), target); err != nil {
		t.Fatalf("VerifyCleanupTargetUnchanged on a harness-only worktree: %v", err)
	}

	ctx, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCleanup()
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatalf("remove harness-only worktree: %v", err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove the worktree: %v", err)
	}
	assertRecoveryInventoryEmpty(t, layout)
}

// TestIntegrationRecoveryCleanupRetainsTrackedResultFileName is the safety
// constraint from the same fix: the exclusion applies to UNTRACKED instances
// only. A repository that legitimately tracks a file named like a stage result
// file, modified in the worktree, is still captured in full.
func TestIntegrationRecoveryCleanupRetainsTrackedResultFileName(t *testing.T) {
	testdep.Require(t, "git")
	layout, workspace, cleanup := seedHarnessArtifactWorktree(t, "claimed-item.json")
	defer cleanup()

	executor.ExcludeStageArtifacts(t.Context(), workspace.Path, "claimed-item.json")
	writeHarnessArtifact(t, workspace.Path, "claimed-item.json", "{\"tracked\":\"edited\"}\n")

	if status := recoveryCLIGit(t, workspace.Path, "status", "--porcelain=v1"); !strings.Contains(status, "claimed-item.json") {
		t.Fatalf("a tracked change to the result-file name was hidden by the exclusion: %q", status)
	}
	ctx, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCleanup()
	if err := workspace.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatalf("remove worktree with tracked work: %v", err)
	}
	assertRecoveryCapturedPath(t, layout)
}

// seedHarnessArtifactWorktree builds an instance, a source repo (optionally
// committing the named files first), and one run worktree wired to the real
// recovery cleanup guard.
func seedHarnessArtifactWorktree(t *testing.T, tracked ...string) (instance.Layout, *worktree.Worktree, func()) {
	t.Helper()
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	source, workcopies := t.TempDir(), t.TempDir()
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previousCloneURL })
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	writeHarnessArtifact(t, source, ".gitignore", "ignored.out\nnode_modules/\n")
	recoveryCLIGit(t, source, "add", ".gitignore")
	recoveryCLIGit(t, source, "commit", "-m", "base")
	for _, name := range tracked {
		writeHarnessArtifact(t, source, name, "{\"tracked\":true}\n")
		recoveryCLIGit(t, source, "add", name)
	}
	if len(tracked) > 0 {
		recoveryCLIGit(t, source, "commit", "-m", "track harness-shaped names")
	}

	const runID = "harness-artifacts"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation",
		WorkflowVersion: 1, Gaggle: "example", StartedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	option, err := recoveryCleanupOption(layout, cfg, workcopies, func(apiv1.RepoRef) (string, error) { return source, nil }, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(workcopies)
	if err != nil {
		t.Fatal(err)
	}
	option(manager)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	workspace, err := manager.Create(ctx, worktree.CreateOptions{
		RepoURL: source, RunID: runID + "-stage", OwnerRunID: runID,
		BaseRef: "main", Branch: "goobers/implementation/" + runID,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return layout, workspace, func() {
		cancel()
		_ = run.Close()
	}
}

func writeHarnessArtifact(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryInventoryEmpty(t *testing.T, layout instance.Layout) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("harness-only cleanup consumed a recovery inventory slot: %s", entry.Name())
		}
	}
}

// assertRecoveryCapturedPath proves exactly one recovery entry exists and
// carries a snapshot bundle, so the safety case asserts the tracked work was
// kept rather than merely that a directory appeared.
func assertRecoveryCapturedPath(t *testing.T, layout instance.Layout) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(layout.Root, "recovery"))
	if err != nil {
		t.Fatal(err)
	}
	captured := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		captured++
		bundle := filepath.Join(layout.Root, "recovery", entry.Name(), "snapshot.bundle")
		if _, statErr := os.Stat(bundle); statErr != nil {
			t.Fatalf("recovery entry %s has no snapshot bundle: %v", entry.Name(), statErr)
		}
	}
	if captured != 1 {
		t.Fatalf("tracked work produced %d recovery entries, want 1", captured)
	}
}
