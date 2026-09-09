//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationPodRecoveryTransfersDirtyWorkspaceBeforeAcknowledgment(t *testing.T) {
	testdep.Require(t, "git")
	for name, repo := range map[string]providers.RepositoryRef{
		"github":            {Provider: "github", Owner: "example", Name: "repo"},
		"github-enterprise": {Provider: "github", URL: "https://forge.example/api/v3", Owner: "example", Name: "repo"},
		"gitea":             {Provider: "gitea", URL: "https://forge.example/service", Owner: "example", Name: "repo"},
		"ado":               {Provider: "ado", URL: "https://ado.example/collection", Owner: "example", Project: "project", Name: "repo"},
	} {
		t.Run(name, func(t *testing.T) { testPodRecoveryRepositoryTransfer(t, repo) })
	}
}

func testPodRecoveryRepositoryTransfer(t *testing.T, repo providers.RepositoryRef) {
	t.Helper()
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	file := filepath.Join(source, "implementation.txt")
	if err := os.WriteFile(file, []byte("unfinished pod implementation"), 0600); err != nil {
		t.Fatal(err)
	}
	host := instance.NewLayout(initDemo(t))
	inventory, err := prepareRecoveryInventory(host.Root)
	if err != nil {
		t.Fatal(err)
	}
	hostRepository := t.TempDir()
	recoveryCLIGit(t, hostRepository, "init", "--bare")
	const runID = "pod-recovery-upload"
	key := repo.CanonicalKey()
	var allow atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer parent-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/claims/list" {
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": []localscheduler.ClaimEntry{{
				RunID: runID, Gaggle: "web", ItemID: "42", ExpiresAt: time.Now().Add(time.Hour),
			}}})
			return
		}
		if !allow.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _, err := recovery.AcceptArchive(r.Context(), r.Body, recovery.RetentionRequest{
			Repository: hostRepository, RepositoryKey: key, RunID: runID,
			IdentityTime: time.Now().UTC(), RetainUntil: time.Now().UTC().Add(30 * 24 * time.Hour),
			InventoryRoot: inventory, CleanupRoots: []string{source}, MaxSnapshots: 128, MaxArchiveBytes: 512 << 20,
		}, recoveryCleanupJournal{directory: host.SchedulerDir(), scrubber: journal.NewRegistryScrubber()})
		if err != nil {
			t.Errorf("host intake: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	for name, value := range map[string]string{
		dispatcher.EnvStageWorkspace: "repo", dispatcher.EnvRunID: runID, dispatcher.EnvGaggle: "web",
		dispatcher.EnvDaemonAPI: server.URL, dispatcher.EnvPodToken: "parent-token",
		executor.RepoProviderEnvVar: string(repo.Provider), executor.RepoOwnerEnvVar: repo.Owner, executor.RepoNameEnvVar: repo.Name,
		executor.RepoBaseURLEnvVar: repo.URL, executor.RepoProjectEnvVar: repo.Project,
		executor.BaseBranchEnvVar: "main",
	} {
		t.Setenv(name, value)
	}
	staging := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, staging)
	}
	if err := publishPodRecovery(t.Context(), source); err == nil {
		t.Fatal("failed upload acknowledged recovery")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "unfinished pod implementation" {
		t.Fatalf("failed upload changed source: %q %v", data, err)
	}
	allow.Store(true)
	if err := publishPodRecovery(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	entries, err := recovery.ReadInventory(t.Context(), inventory, 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("host custody: %+v %v", entries, err)
	}
	if got := recoveryCLIGit(t, hostRepository, "show", entries[0].Record.Ref+":implementation.txt"); got != "unfinished pod implementation" {
		t.Fatalf("host archive content: %q", got)
	}
}
