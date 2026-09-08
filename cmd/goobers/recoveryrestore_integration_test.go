//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
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
	for _, mode := range []string{"record", "issue", "http-issue", "resume-issue", "resume-prepared", "resume-adopted"} {
		t.Run(mode, func(t *testing.T) { testRecoveryRestoreCommand(t, mode) })
	}
}

func testRecoveryRestoreCommand(t *testing.T, mode string) {
	resume := strings.HasPrefix(mode, "resume-")
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
	if mode != "record" {
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
		if mode == "http-issue" {
			serveRecoveryRestoreFixture(t, layout, identity)
		}
	}
	stdout.Reset()
	stderr.Reset()
	target := "operator-recovery"
	invoke := func() int { return runRecoveryRestore(args, &stdout, &stderr) }
	if resume {
		layout := instance.NewLayout(root)
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			t.Fatal(err)
		}
		if ok, _, err := ledger.Claim("7", "receiving-run", "implementation-recovery", time.Hour); err != nil || !ok {
			t.Fatalf("receiving claim: %t %v", ok, err)
		}
		seedItemRepositoryForTest(t, layout, "receiving-run", "7", identity)
		t.Setenv("GOOBERS_RUN_ID", "receiving-run")
		t.Setenv("GOOBERS_WORKFLOW", "implementation-recovery")
		target = providers.BranchName("implementation-recovery", "receiving-run")
		recoveryCLIGit(t, destination, "checkout", "-b", target)
		if mode == "resume-prepared" || mode == "resume-adopted" {
			registry, _ := journal.DefaultScrubber()
			prepared, err := restoreIssueRecovery(context.Background(), layout, identity.CanonicalKey(), "7", destination, "goobers/recovery-resume/receiving-run", registry)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "resume-adopted" {
				if err := recovery.AdoptRestoredCommit(context.Background(), destination, target, base, prepared); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Chdir(destination)
		invoke = func() int { return runRecoveryResume([]string{root}, &stdout, &stderr) }
	}
	if code := invoke(); code != 0 {
		t.Fatalf("restore command returned %d: %s", code, stderr.String())
	}
	if parent := recoveryCLIGit(t, destination, "rev-parse", target+"^"); parent != main {
		t.Fatalf("restore used stale main: %s != %s", parent, main)
	}
	if data := recoveryCLIGit(t, destination, "show", target+":implementation.txt"); data != "retained implementation" {
		t.Fatalf("wrong restored content: %q", data)
	}
	wantHead := base
	if resume {
		wantHead = recoveryCLIGit(t, destination, "rev-parse", target)
		data, err := os.ReadFile(filepath.Join(destination, "implementation.txt"))
		if err != nil || string(data) != "retained implementation" {
			t.Fatalf("receiving run did not consume restored work: %q %v", data, err)
		}
	}
	if head := recoveryCLIGit(t, destination, "rev-parse", "HEAD"); head != wantHead {
		t.Fatal("restore moved the current checkout")
	}
	if status := recoveryCLIGit(t, destination, "status", "--porcelain"); status != "" {
		t.Fatalf("restore changed checkout/index: %s", status)
	}
	before := recoveryCLIGit(t, destination, "rev-parse", target)
	wantRetryCode := 1
	if resume {
		wantRetryCode = 0
		if refs := recoveryCLIGit(t, destination, "for-each-ref", "--format=%(refname)", "refs/heads/goobers/recovery-resume/"); refs != "" {
			t.Fatalf("completed adoption retained preparation branches: %s", refs)
		}
		// A replay acknowledges the verified effect, without reapplying it
		// onto a newer main or manufacturing a second implementation commit.
		recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "main after adoption")
	}
	if code := invoke(); code != wantRetryCode {
		t.Fatalf("existing branch was not refused: %d", code)
	}
	if after := recoveryCLIGit(t, destination, "rev-parse", target); after != before {
		t.Fatal("retry overwrote operator branch")
	}
}

// Exercise the production delivery service and download/restore command with
// actual Git objects. The HTTP API's scoped-auth middleware is tested separately
// in httpapi; this fixture checks the claims bearer before invoking the service.
func serveRecoveryRestoreFixture(t *testing.T, layout instance.Layout, repo providers.RepositoryRef) {
	t.Helper()
	const runID = "receiving-run"
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", runID, "implementation-recovery", time.Hour); err != nil || !ok {
		t.Fatalf("receiving claim: %t %v", ok, err)
	}
	seedItemRepositoryForTest(t, layout, runID, "7", repo)
	service := recoveryDeliveryService{layout: layout}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs/"+runID+"/recovery" || r.Header.Get("Authorization") != "Bearer recovery-test-claims" {
			t.Error("unexpected recovery request")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := service.StreamRecovery(r.Context(), runID, r.URL.Query().Get("repositoryKey"), r.URL.Query().Get("issue"), w); err != nil {
			t.Errorf("delivery service failed: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(claimsclient.EnvEndpoint, server.URL)
	t.Setenv(claimsclient.EnvToken, "recovery-test-claims")
	t.Setenv(claimsclient.EnvRunID, runID)
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "resolved-stage-recovery-token")
}
