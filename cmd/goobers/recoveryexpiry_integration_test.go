//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryExpirySweepRemovesOnlyEligibleOwnedState(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"delete", "dry-run", "running", "busy", "conflict", "renewed", "abandoned", "abandoned-renewed", "abandoned-stage", "abandoned-running"} {
		t.Run(mode, func(t *testing.T) { testRecoveryExpirySweep(t, mode) })
		t.Run("pinned-"+mode, func(t *testing.T) { testRecoveryExpirySweep(t, "pinned-"+mode) })
	}
	for _, mode := range []string{"landed", "landed-dry-run", "landed-unconfirmed", "landed-wrong-content", "landed-missing-object"} {
		t.Run(mode, func(t *testing.T) { testRecoveryExpirySweep(t, mode) })
		t.Run("pinned-"+mode, func(t *testing.T) { testRecoveryExpirySweep(t, "pinned-"+mode) })
	}
}

func testRecoveryExpirySweep(t *testing.T, mode string) {
	t.Helper()
	pinned := strings.HasPrefix(mode, "pinned-")
	mode = strings.TrimPrefix(mode, "pinned-")
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
	manager, err := worktree.NewManager(filepath.Join(layout.Root, "workcopies"), worktree.WithPinnedRoot(filepath.Join(layout.Root, "pinned")))
	if err != nil {
		t.Fatal(err)
	}
	var mirror string
	if pinned {
		lease, acquireErr := manager.AcquirePinned(ctx, worktree.PinnedOptions{RepoURL: source, RunID: "expiry-integration", BaseRef: "main", Branch: "operator-branch"})
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		defer func() { _ = lease.Release() }()
		mirror = lease.Worktree.Path
		if mode != "busy" {
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		mirror, err = manager.WorkingCopy(ctx, source)
	}
	if err != nil {
		t.Fatal(err)
	}
	previous := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previous })
	now := time.Now().UTC()
	start, finish := now.Add(-90*24*time.Hour), now.Add(-45*24*time.Hour)
	if strings.HasPrefix(mode, "abandoned") || strings.HasPrefix(mode, "landed") {
		finish = now.Add(-time.Hour)
	}
	const runID = "expiry-integration"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: start}, nil, journal.WithClock(func() time.Time { return finish }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if mode != "running" && mode != "abandoned-running" {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
	}
	if mode != "busy" || pinned {
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
	retained, path, err := recovery.PublishToInventory(ctx, mirror, root, []string{manager.Root}, record, 128, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mode, "landed") {
		seedRecoveryLandingForRetirement(t, layout, mirror, retained, mode)
	}
	if strings.HasPrefix(mode, "abandoned") {
		event, err := recovery.AbandonedEvent(retained)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "abandoned-stage" {
			event.Runner[livejournal.EmitKeyRunnerField] = "untrusted-stage"
		}
		log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		appendErr := log.Append(event)
		closeErr := log.Close()
		if appendErr != nil || closeErr != nil {
			t.Fatalf("abandonment journal: %v %v", appendErr, closeErr)
		}
		if mode == "abandoned-renewed" {
			if _, err := recovery.RenewRetention(ctx, path, retained.RetainUntil.Add(time.Hour), 1<<20); err != nil {
				t.Fatal(err)
			}
		}
	}
	if mode == "renewed" {
		if _, err := recovery.RenewRetention(ctx, path, now.Add(time.Hour), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "conflict" {
		recoveryCLIGit(t, mirror, "update-ref", ref, base)
	}
	setup := &schedulerSetup{Config: &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "team", Name: "repo"}}, Retention: instance.RetentionConfig{Enabled: true, DryRun: mode == "dry-run" || mode == "landed-dry-run"}}, LegacyWorktrees: manager}
	var stdout, stderr bytes.Buffer
	err = pruneConfiguredRetention(ctx, layout, setup, &stdout, &stderr)
	if (err != nil) != (mode == "conflict" || mode == "landed-missing-object" || pinned && mode == "busy") {
		t.Fatalf("sweep: %v stderr=%s", err, stderr.String())
	}
	gotRef := recoveryCLIGit(t, mirror, "for-each-ref", "--format=%(objectname)", ref)
	if mode == "delete" || mode == "abandoned" || mode == "landed" {
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
		if pinned && branch == "main" {
			branch = "refs/remotes/mirror/main"
		}
		if got := recoveryCLIGit(t, mirror, "rev-parse", branch); got != want {
			t.Fatalf("operator branch %s changed: %s", branch, got)
		}
	}
}

func seedRecoveryLandingForRetirement(t *testing.T, layout instance.Layout, repository string, record recovery.Record, mode string) {
	t.Helper()
	restored, err := recovery.RestoreSnapshot(t.Context(), repository, record, record.BaseSHA, "receiving-restoration", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tree := recoveryCLIGit(t, repository, "rev-parse", restored+"^{tree}")
	if mode == "landed-wrong-content" {
		tree = recoveryCLIGit(t, repository, "rev-parse", record.BaseSHA+"^{tree}")
	}
	merge := recoveryCLIGit(t, repository, "commit-tree", tree, "-p", record.BaseSHA, "-m", "squash merge")
	if mode == "landed-missing-object" {
		merge = strings.Repeat("f", 40)
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "team", Name: "repo"}
	route, err := recoveryLandingRoute(repo)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "receiving-run", Workflow: "recovery", WorkflowVersion: 1, WorkspaceRepository: &repo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	intent := providers.LandingIntent{ID: "landing-intent", Operation: "merge", RepositoryAPIURL: route, PullID: "7", ExpectedHeadSHA: restored}
	confirmation := providers.MergeConfirmation{IntentID: intent.ID, RepositoryAPIURL: route, PullID: "7", MergeSHA: merge}
	fields := []map[string]any{providers.MutationRunnerFields("merge-intent", nil, nil, &intent)}
	if mode != "landed-unconfirmed" {
		fields = append(fields, providers.MutationRunnerFields("merge", &confirmation, nil, nil))
	}
	for _, runnerFields := range fields {
		if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "7"}, Runner: runnerFields}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
}
