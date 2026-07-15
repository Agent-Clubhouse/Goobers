package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenPRCreatesThenUpdatesOnRepass is #132's core CLI-level acceptance:
// invoking `goobers open-pr` via the actual CLI entrypoint opens a PR, writes
// prNumber/pull-request-url to the declared result file (the #132 handoff
// mechanism a downstream ci-poll stage's Task.InputsFrom consumes), and a
// second call for the same run (a repass) updates the same PR instead of
// attempting a duplicate.
func TestOpenPRCreatesThenUpdatesOnRepass(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "pr #1") {
		t.Fatalf("stdout = %q, want a mention of the opened PR", stdout)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "pr-result.json"))
	if err != nil {
		t.Fatalf("read pr-result.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal pr-result.json: %v", err)
	}
	if result["prNumber"] != "1" {
		t.Fatalf("prNumber = %q, want \"1\"", result["prNumber"])
	}
	if result["pull-request-url"] == "" {
		t.Fatalf("pull-request-url missing: %+v", result)
	}

	server.mu.Lock()
	prCountAfterFirst := len(server.prs)
	server.mu.Unlock()
	if prCountAfterFirst != 1 {
		t.Fatalf("expected exactly one PR after the first call, got %d", prCountAfterFirst)
	}

	// Repass: same run, same stable branch — must update, not duplicate.
	workDir2 := t.TempDir()
	t.Chdir(workDir2)
	code, stdout, stderr = runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr (repass): code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "pr #1") {
		t.Fatalf("repass stdout = %q, want the SAME pr #1, not a new one", stdout)
	}
	server.mu.Lock()
	prCountAfterRepass := len(server.prs)
	server.mu.Unlock()
	if prCountAfterRepass != 1 {
		t.Fatalf("expected still exactly one PR after the repass, got %d (a duplicate was created)", prCountAfterRepass)
	}
}

// TestOpenPRMissingRunIDFailsClosed proves open-pr refuses to run without a
// real run identity — no meaningful branch/PR to open otherwise.
func TestOpenPRMissingRunIDFailsClosed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	// #321: a live local-ci `go test ./...` inherits the run's real
	// GOOBERS_RUN_ID from buildStageEnv, defeating this fail-closed test.
	// Simulate the parent-process leak, then clear it — genuinely exercises the
	// missing-run-id path and regression-guards the fix under normal CI.
	t.Setenv("GOOBERS_RUN_ID", "ambient-parent-leak")
	unsetRunID(t)
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing run context), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "GOOBERS_RUN_ID") {
		t.Fatalf("stderr = %q, want a clear missing-run-id message", stderr)
	}
}
