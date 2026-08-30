package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ephemeraltmp_test.go covers the self-runner binding of the `tmp:ephemeral`
// restriction at the seam that actually runs a stage.
//
// The defect it guards: the API pod on the prod AKS instance is both the
// control plane and the CI executor, so every stage placed on runner `self`
// wrote its Go build cache into the daemon's own long-lived `/tmp` and nothing
// ever reclaimed it — 9.5 GB and climbing at ~45 MiB/min against a 10Gi memory
// cgroup, until the OOM killer took pid 1. A dispatched stage pod never had the
// problem, because a fresh pod gets a fresh emptyDir. These tests assert the
// self path now gets the same lifetime.

// ephemeralStageOutput runs a stage that reports the temp and cache locations
// it was handed and writes a marker into each, and returns the stdout it
// captured.
const ephemeralStageScript = `printf '%s\n%s\n' "$TMPDIR" "$GOCACHE"; mkdir -p "$GOCACHE"; ` +
	`echo tempbytes > "$TMPDIR/marker"; echo cachebytes > "$GOCACHE/entry"`

func reportedPaths(t *testing.T, stdout string) (tmpdir, gocache string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("stage stdout %q does not carry the two reported paths", stdout)
	}
	return lines[0], lines[1]
}

// TestShellExecutorBindsTmpEphemeralPerAttempt is the whole effect in one
// pass: the stage's temp and its temp-nested build cache both resolve inside
// an attempt-private directory that did not exist before the stage and does
// not exist after it.
func TestShellExecutorBindsTmpEphemeralPerAttempt(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	root := t.TempDir()
	daemonCache := filepath.Join(root, "gocache")
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", daemonCache)

	exec.EphemeralTmp = true
	exec.EphemeralTmpRoot = root
	env := baseEnvelope(t)

	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", ephemeralStageScript}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (result %+v)", result.Status, result)
	}

	tmpdir, gocache := reportedPaths(t, string(rec.recorded["task-1/stdout.log"]))
	if tmpdir == root || tmpdir == "" {
		t.Fatalf("TMPDIR = %q, want an attempt-private directory beneath the temp root %q", tmpdir, root)
	}
	if filepath.Dir(tmpdir) != root {
		t.Fatalf("TMPDIR = %q, want it carved directly out of the temp root %q", tmpdir, root)
	}
	if gocache != filepath.Join(tmpdir, "gocache") {
		t.Fatalf("GOCACHE = %q, want the temp-nested cache re-rooted to %q", gocache, filepath.Join(tmpdir, "gocache"))
	}

	// The bytes the stage wrote are gone, and so is the directory that held
	// them — the reclaim half of the effect.
	if _, err := os.Stat(tmpdir); !os.IsNotExist(err) {
		t.Fatalf("the attempt's temp %q survived the stage (stat err %v)", tmpdir, err)
	}
	if _, err := os.Stat(daemonCache); !os.IsNotExist(err) {
		t.Fatalf("the daemon's shared %q was written to; the stage's cache must never land there under tmp:ephemeral", daemonCache)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("temp root holds %d entries after the stage, %d before: the attempt left residue", len(after), len(before))
	}
}

// TestShellExecutorReclaimsTmpEphemeralWhenTheStageFails: an unbounded cache
// is grown by failing builds exactly as fast as by passing ones, so reclaim
// that only runs on success is not reclaim.
func TestShellExecutorReclaimsTmpEphemeralWhenTheStageFails(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", filepath.Join(root, "gocache"))

	exec.EphemeralTmp = true
	exec.EphemeralTmpRoot = root

	result, err := exec.Run(context.Background(), baseEnvelope(t), apiv1.DeterministicRun{
		Command: []string{"sh", "-c", ephemeralStageScript + "; exit 3"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}

	tmpdir, _ := reportedPaths(t, string(rec.recorded["task-1/stdout.log"]))
	if _, err := os.Stat(tmpdir); !os.IsNotExist(err) {
		t.Fatalf("a failed stage left its temp %q behind (stat err %v)", tmpdir, err)
	}
}

// TestShellExecutorTmpEphemeralPreservesWorkspaceAndConcurrentAttempts pins
// the two things reclaim must never reach: the run's own workspace — the
// worktree whose delta later stages consume — and another attempt's temp. The
// API pod runs stages concurrently, so a reclaim that swept the temp root
// would delete a live run's cache mid-compile.
func TestShellExecutorTmpEphemeralPreservesWorkspaceAndConcurrentAttempts(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", filepath.Join(root, "gocache"))

	exec.EphemeralTmp = true
	exec.EphemeralTmpRoot = root

	env := baseEnvelope(t)
	// Workspace continuity: committed content, an uncommitted edit, and a
	// nested directory — everything a following stage would expect to still
	// be there.
	workspaceFile := filepath.Join(env.Workspace, "src", "main.go")
	if err := os.MkdirAll(filepath.Dir(workspaceFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceFile, []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A concurrent attempt's temp, standing in the same root, holding bytes a
	// live build is depending on.
	concurrent, err := os.MkdirTemp(root, "goobers-ephemeral-tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	concurrentEntry := filepath.Join(concurrent, "gocache", "entry")
	if err := os.MkdirAll(filepath.Dir(concurrentEntry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concurrentEntry, []byte("still building"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", ephemeralStageScript}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}

	if got, err := os.ReadFile(workspaceFile); err != nil || string(got) != "package main" {
		t.Fatalf("workspace content = %q, err %v; the binding must not touch the workspace", got, err)
	}
	if got, err := os.ReadFile(concurrentEntry); err != nil || string(got) != "still building" {
		t.Fatalf("a concurrent attempt's cache = %q, err %v; reclaim must never cross runs", got, err)
	}
	// And nothing of the stage's own temp was placed in the workspace: a build
	// cache materializing there would surface as untracked worktree content in
	// the run's delta.
	entries, err := os.ReadDir(env.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "goobers-ephemeral-tmp-") || entry.Name() == "gocache" {
			t.Fatalf("the binding placed %q inside the workspace", entry.Name())
		}
	}
}

// TestShellExecutorWithoutTmpEphemeralIsUnchanged is the zero-declaration
// invariance guard (goobernetes-architecture.md §11 item 1): an instance that
// declares no runners — the default everywhere — must see the stage
// environment it has always seen.
func TestShellExecutorWithoutTmpEphemeralIsUnchanged(t *testing.T) {
	exec, rec := newTestExecutor(t, nil)
	root := t.TempDir()
	daemonCache := filepath.Join(root, "gocache")
	t.Setenv("TMPDIR", root)
	t.Setenv("GOCACHE", daemonCache)

	if exec.EphemeralTmp {
		t.Fatal("EphemeralTmp must default to false")
	}

	result, err := exec.Run(context.Background(), baseEnvelope(t), apiv1.DeterministicRun{Command: []string{"sh", "-c", ephemeralStageScript}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}

	tmpdir, gocache := reportedPaths(t, string(rec.recorded["task-1/stdout.log"]))
	if tmpdir != root {
		t.Fatalf("TMPDIR = %q, want the daemon's own temp root %q unchanged", tmpdir, root)
	}
	if gocache != daemonCache {
		t.Fatalf("GOCACHE = %q, want %q unchanged", gocache, daemonCache)
	}
	if _, err := os.Stat(filepath.Join(daemonCache, "entry")); err != nil {
		t.Fatalf("without the restriction the cache must persist exactly as before: %v", err)
	}
}

// TestShellExecutorTmpEphemeralFailsClosed: the solver has already told the
// operator that runner `self` enforces this effect. A stage that cannot get
// its private temp must be refused, not quietly run against the shared one.
func TestShellExecutorTmpEphemeralFailsClosed(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec.EphemeralTmp = true
	exec.EphemeralTmpRoot = filepath.Join(blocker, "under-a-file")

	_, err := exec.Run(context.Background(), baseEnvelope(t), apiv1.DeterministicRun{Command: []string{"sh", "-c", "echo ran"}})
	if err == nil {
		t.Fatal("Run returned no error; an unbindable restriction must refuse the stage")
	}
	if !strings.Contains(err.Error(), "tmp:ephemeral") {
		t.Fatalf("error %q does not name the restriction that could not be bound", err)
	}
}
