//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func recoveryCLIGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repository}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Recovery Test", "GIT_AUTHOR_EMAIL=recovery@example.invalid", "GIT_COMMITTER_NAME=Recovery Test", "GIT_COMMITTER_EMAIL=recovery@example.invalid")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git failed: %v: %s", err, data)
	}
	return strings.TrimSpace(string(data))
}

func TestIntegrationRecoveryRestoreCommandUsesFreshMainAndPreservesCheckout(t *testing.T) {
	testdep.Require(t, "git")
	for _, byIssue := range []bool{false, true} {
		name := "record"
		if byIssue {
			name = "issue"
		}
		t.Run(name, func(t *testing.T) { testRecoveryRestoreCommand(t, byIssue) })
	}
}

func testRecoveryRestoreCommand(t *testing.T, byIssue bool) {
	t.Setenv("GOOBERS_GITHUB_TOKEN", "local-only-recovery-fixture-token")
	root := initDemo(t)
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	configured := cfg.Repos[0]
	identity := providers.RepositoryRef{Provider: providers.ProviderKind(configured.Provider), URL: configured.BaseURL, Owner: configured.Owner, Project: configured.Project, Name: configured.Name}
	remote, err := runner.DefaultRepoCloneURL(apiv1.RepoRef{Provider: apiv1.Provider(configured.Provider), BaseURL: configured.BaseURL, Owner: configured.Owner, Project: configured.Project, Name: configured.Name})
	if err != nil {
		t.Fatal(err)
	}
	source, destination, retained := t.TempDir(), t.TempDir(), t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	base := recoveryCLIGit(t, source, "rev-parse", "HEAD")
	recoveryCLIGit(t, destination, "clone", source, ".")
	// Redirect only this destination's exact configured URL to the local
	// fixture. The production command still derives its URL from config.
	recoveryCLIGit(t, destination, "config", "url."+source+".insteadOf", remote)
	if err := os.WriteFile(filepath.Join(source, "implementation.txt"), []byte("retained implementation"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-45 * 24 * time.Hour)
	record := recovery.Record{Version: 1, RunID: "cli-recovery", RepositoryKey: identity.CanonicalKey(), BaseSHA: base, CreatedAt: now, RetainUntil: now.Add(time.Hour)}
	record.SnapshotSHA, err = recovery.CaptureSnapshot(context.Background(), source, record.RunID, now)
	if err != nil {
		t.Fatal(err)
	}
	record.Ref, err = recovery.RefForSnapshot(record.RunID, record.SnapshotSHA)
	if err != nil {
		t.Fatal(err)
	}
	record.PatchDigest, err = recovery.WriteSnapshotPatch(context.Background(), source, base, record.SnapshotSHA, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.PublishRetainedState(context.Background(), source, retained, []string{source}, record, 1<<20); err != nil {
		t.Fatal(err)
	}
	// Advance main without committing the retained implementation file.
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "advance main")
	main := recoveryCLIGit(t, source, "rev-parse", "HEAD")
	args := []string{"--record", filepath.Join(retained, recovery.RecordFileName), "--repository", destination, "--branch", "operator-recovery", root}
	var stdout, stderr bytes.Buffer
	if code := runRecoveryRestore(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "retention deadline has expired") {
		t.Fatalf("unrenewed expired archive was not refused: %d %s", code, stderr.String())
	}
	if _, err := recovery.RenewRetention(context.Background(), filepath.Join(retained, recovery.RecordFileName), time.Now().UTC().Add(30*24*time.Hour), 1<<20); err != nil {
		t.Fatal(err)
	}
	if byIssue {
		layout := instance.NewLayout(root)
		inventory, err := prepareRecoveryInventory(root)
		if err != nil {
			t.Fatal(err)
		}
		_, path, err := recovery.PublishToInventory(context.Background(), source, inventory, []string{source}, record, 128, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recovery.RenewRetention(context.Background(), path, time.Now().UTC().Add(30*24*time.Hour), 1<<20); err != nil {
			t.Fatal(err)
		}
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: record.RunID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: now}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		seedItemRepositoryForTest(t, layout, record.RunID, "7", identity)
		args = []string{"--issue", "7", "--repository-key", identity.CanonicalKey(), "--repository", destination, "--branch", "operator-recovery", root}
	}
	stdout.Reset()
	stderr.Reset()
	if code := runRecoveryRestore(args, &stdout, &stderr); code != 0 {
		t.Fatalf("restore command returned %d: %s", code, stderr.String())
	}
	if parent := recoveryCLIGit(t, destination, "rev-parse", "operator-recovery^"); parent != main {
		t.Fatalf("restore used stale main: %s != %s", parent, main)
	}
	if data := recoveryCLIGit(t, destination, "show", "operator-recovery:implementation.txt"); data != "retained implementation" {
		t.Fatalf("wrong restored content: %q", data)
	}
	if head := recoveryCLIGit(t, destination, "rev-parse", "HEAD"); head != base {
		t.Fatal("restore moved the current checkout")
	}
	if status := recoveryCLIGit(t, destination, "status", "--porcelain"); status != "" {
		t.Fatalf("restore changed checkout/index: %s", status)
	}
	before := recoveryCLIGit(t, destination, "rev-parse", "operator-recovery")
	if code := runRecoveryRestore(args, &stdout, &stderr); code != 1 {
		t.Fatalf("existing branch was not refused: %d", code)
	}
	if after := recoveryCLIGit(t, destination, "rev-parse", "operator-recovery"); after != before {
		t.Fatal("retry overwrote operator branch")
	}
}
