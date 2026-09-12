package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workerhost"
)

func TestRunWorkerUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--warp-speed"}},
		{name: "positional args", args: []string{"extra"}},
		{name: "empty task queue", args: []string{"--task-queue", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runWorker(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2 (usage error)\nstderr: %s", code, stderr.String())
			}
		})
	}
}

func TestRunWorkerHelpComesFromRegistry(t *testing.T) {
	command, ok := commandHelp("worker")
	if !ok {
		t.Fatal("worker is not registered")
	}
	if command.long != workerHelp {
		t.Fatal("worker registry help drifted from workerHelp")
	}
	if command.synopsis == "" {
		t.Fatal("worker has no top-level synopsis")
	}
	var stderr bytes.Buffer
	if code := runWorker([]string{"--warp-speed"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage: goobers worker") {
		t.Fatalf("usage output does not render the registered help:\n%s", stderr.String())
	}
}

func TestWorkerEngineDepsWiresWorkspacesAndAutomated(t *testing.T) {
	engineRuntime, err := workerEngineDeps(filepath.Join(t.TempDir(), "work"))
	if err != nil {
		t.Fatalf("workerEngineDeps: %v", err)
	}
	t.Cleanup(func() { _ = engineRuntime.Close() })
	deps := engineRuntime.deps
	if deps.Workspaces == nil {
		t.Error("no workspace provisioner wired — every workspace stage would fail closed")
	}
	if deps.Auto == nil {
		t.Error("no automated gate evaluator wired")
	}
	if deps.Scrubber == nil {
		t.Error("no registry-backed scrubber wired")
	}
	// Agentic/deterministic seams deliberately await the runtime wiring slice.
	if deps.Goober != nil || deps.Det != nil {
		t.Error("executor seams unexpectedly wired; update the worker help text and this test together")
	}
}

func TestWorkerSeamScrubberUsesItsLiveRegistry(t *testing.T) {
	const secret = "SUPER-SECRET-CANARY-9f8e7d6c5b4a3210"
	registry, scrubber := journal.DefaultScrubber()
	seams := &workerSeams{shared: registry, scrubber: scrubber}
	seams.SharedRegistry().Register([]byte(secret))

	got := seams.Scrubber().Scrub([]byte("worker result: " + secret))
	if bytes.Contains(got, []byte(secret)) || !bytes.Contains(got, []byte(journal.Redacted)) {
		t.Fatalf("live worker seam scrubber did not use its registry: %q", got)
	}
}

func TestWireWorkerRuntimeSeamsAssignsLiveScrubber(t *testing.T) {
	const secret = "SUPER-SECRET-CANARY-9f8e7d6c5b4a3210"
	registry, scrubber := journal.DefaultScrubber()
	seams := &workerSeams{shared: registry, scrubber: scrubber}
	var deps bootstrap.EngineDeps
	wireWorkerRuntimeSeams(&deps, seams, t.TempDir())
	registry.Register([]byte(secret))

	got := deps.Scrubber.Scrub([]byte("worker result: " + secret))
	if bytes.Contains(got, []byte(secret)) || !bytes.Contains(got, []byte(journal.Redacted)) {
		t.Fatalf("wired engine scrubber did not use the worker's live registry: %q", got)
	}
	if deps.Canary != registry {
		t.Fatal("wired canary and scrubber do not share the worker registry")
	}
}

func TestWorkerEngineDepsWindowsPreflightsPathLength(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source")
	deepest := filepath.Join("deep", strings.Repeat("x", 80), "file.txt")
	if err := os.MkdirAll(filepath.Join(src, filepath.Dir(deepest)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, deepest), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"add", "."},
		{"commit", "-m", "initial"},
	} {
		cmd := testgit.Command(args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	workRoot := filepath.Join(t.TempDir(), strings.Repeat("w", 100))
	engineRuntime, err := workerEngineDepsForPlatform(workRoot, "windows", "test-worker")
	if err != nil {
		t.Fatalf("workerEngineDepsForPlatform: %v", err)
	}
	t.Cleanup(func() { _ = engineRuntime.Close() })
	deps := engineRuntime.deps
	workspaces, ok := deps.Workspaces.(*workerhost.WorktreeWorkspaces)
	if !ok {
		t.Fatalf("workspaces = %T, want *workerhost.WorktreeWorkspaces", deps.Workspaces)
	}
	workspaces.CloneURL = func(apiv1.RepoRef) (string, error) { return src, nil }
	_, err = workspaces.Provision(context.Background(), engine.WorkspaceRequest{
		RunID:    "run-1",
		Stage:    "build",
		Workflow: "implementation",
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub,
			Owner:    "example",
			Name:     "repo",
			Branch:   "main",
		},
	})
	if err == nil {
		t.Fatal("Provision succeeded, want path-length preflight failure")
	}
	if !strings.Contains(err.Error(), deepest) || !strings.Contains(err.Error(), "are available") {
		t.Fatalf("Provision error = %v, want offending path and available budget", err)
	}
	runDirs, err := filepath.Glob(filepath.Join(workRoot, "workcopies", "*", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runDirs) != 0 {
		t.Fatalf("checkout directories created before preflight: %v", runDirs)
	}
}

func TestClaimWorkerRootRejectsAnotherWorker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	first, err := claimWorkerRoot(root, "worker-a")
	if err != nil {
		t.Fatalf("claimWorkerRoot: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	second, err := claimWorkerRoot(root, "worker-a")
	if err == nil {
		_ = second.Release()
		t.Fatal("same-host worker claimed a live worker's root")
	}
	if !strings.Contains(err.Error(), "another live worker") {
		t.Fatalf("concurrent claim error = %v, want live-worker failure", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release first claim: %v", err)
	}
	restarted, err := claimWorkerRoot(root, "worker-a")
	if err != nil {
		t.Fatalf("replacement worker could not recover root: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Release() })
}
