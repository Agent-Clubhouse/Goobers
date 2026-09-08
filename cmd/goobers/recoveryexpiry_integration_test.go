//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryExpirySweepRemovesOnlyEligibleOwnedState(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"delete", "dry-run", "running", "busy", "conflict", "renewed"} {
		t.Run(mode, func(t *testing.T) { testRecoveryExpirySweep(t, mode) })
	}
}

func testRecoveryExpirySweep(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	layout := instance.NewLayout(t.TempDir())
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	base := recoveryCLIGit(t, source, "rev-parse", "HEAD")
	recoveryCLIGit(t, source, "checkout", "-b", "operator-branch")
	if err := os.WriteFile(filepath.Join(source, "implementation.txt"), []byte("retained work"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, source, "add", ".")
	recoveryCLIGit(t, source, "commit", "-m", "implementation")
	snapshot := recoveryCLIGit(t, source, "rev-parse", "HEAD")
	manager, err := worktree.NewManager(filepath.Join(layout.Root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := manager.WorkingCopy(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	previous := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previous })
	now := time.Now().UTC()
	start, finish := now.Add(-90*24*time.Hour), now.Add(-45*24*time.Hour)
	const runID = "expiry-integration"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: start}, nil, journal.WithClock(func() time.Time { return finish }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if mode != "running" {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
	}
	if mode != "busy" {
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := recovery.RefForSnapshot(runID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := recovery.WriteSnapshotPatch(ctx, mirror, base, snapshot, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{Version: 1, RunID: runID, RepositoryKey: "github|||team|repo|", Ref: ref, BaseSHA: base, SnapshotSHA: snapshot, PatchDigest: digest, CreatedAt: start, RetainUntil: finish.Add(30 * 24 * time.Hour)}
	root, err := prepareRecoveryInventory(layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := recovery.PublishToInventory(ctx, mirror, root, []string{manager.Root}, record, 128, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "renewed" {
		if _, err := recovery.RenewRetention(ctx, path, now.Add(time.Hour), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "conflict" {
		recoveryCLIGit(t, mirror, "update-ref", ref, base)
	}
	setup := &schedulerSetup{Config: &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "team", Name: "repo"}}, Retention: instance.RetentionConfig{Enabled: true, DryRun: mode == "dry-run"}}, LegacyWorktrees: manager}
	var stdout, stderr bytes.Buffer
	err = pruneConfiguredRetention(ctx, layout, setup, &stdout, &stderr)
	if (err != nil) != (mode == "conflict") {
		t.Fatalf("sweep: %v stderr=%s", err, stderr.String())
	}
	gotRef := recoveryCLIGit(t, mirror, "for-each-ref", "--format=%(objectname)", ref)
	if mode == "delete" {
		if gotRef != "" {
			t.Fatalf("expired pin remains: %s", gotRef)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("expired archive remains: %v", err)
		}
	} else {
		wantRef := snapshot
		if mode == "conflict" {
			wantRef = base
		}
		if gotRef != wantRef {
			t.Fatalf("protected pin changed: %s != %s", gotRef, wantRef)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected recovery metadata lost: %v", err)
		}
	}
	for branch, want := range map[string]string{"main": base, "operator-branch": snapshot} {
		if got := recoveryCLIGit(t, mirror, "rev-parse", branch); got != want {
			t.Fatalf("operator branch %s changed: %s", branch, got)
		}
	}
}
