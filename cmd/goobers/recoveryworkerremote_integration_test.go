//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRemoteWorkerCleanupWaitsForVerifiedCustody(t *testing.T) {
	testdep.Require(t, "git")
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	configured := cfg.Repos[0]
	repo := apiv1.RepoRef{Provider: apiv1.Provider(configured.Provider), Owner: configured.Owner, Name: configured.Name, Branch: "main"}
	url, err := runner.DefaultRepoCloneURL(repo)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	// Retain the production repository URL/digest while resolving its Git
	// transport to a local fixture, with no network or credential dependency.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+filepath.ToSlash(source)+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", url)
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := instance.NewLayout(initDemo(t))
	hostRepository := t.TempDir()
	recoveryCLIGit(t, hostRepository, "init", "--bare")
	inventory, err := prepareRecoveryInventory(host.Root)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "remote-worker-custody"
	key := (providers.RepositoryRef{Provider: providers.ProviderKind(configured.Provider), Owner: configured.Owner, Name: configured.Name}).CanonicalKey()
	var allow atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer worker-token" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.URL.Path == "/api/v1/claims/list" {
			_ = json.NewEncoder(response).Encode(map[string]any{"entries": []localscheduler.ClaimEntry{{
				RunID: runID, Gaggle: "web", ItemID: "42", ExpiresAt: time.Now().Add(time.Hour),
			}}})
			return
		}
		if !allow.Load() {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _, err := recovery.AcceptArchive(request.Context(), request.Body, recovery.RetentionRequest{
			Repository: hostRepository, RepositoryKey: key, RunID: runID, IdentityTime: time.Now().UTC(),
			RetainUntil: time.Now().UTC().Add(30 * 24 * time.Hour), InventoryRoot: inventory,
			CleanupRoots: []string{manager.Root}, MaxSnapshots: 128, MaxArchiveBytes: 512 << 20,
		}, recoveryCleanupJournal{directory: host.SchedulerDir(), scrubber: journal.NewRegistryScrubber()})
		if err != nil {
			t.Errorf("host intake: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	worker := &workerSeams{root: layout.Root, scrubber: journal.NewRegistryScrubber(),
		recoveryEmitter: &livejournal.HTTPEmitter{BaseURL: server.URL, Token: "worker-token"}}
	if err := worker.installRemoteRecoveryGuard(manager); err != nil {
		t.Fatal(err)
	}
	provisioner := &workerhost.WorktreeWorkspaces{Manager: manager}
	workspace, err := provisioner.Provision(t.Context(), engine.WorkspaceRequest{
		RunID: runID, Stage: "implement", Gaggle: "web", Workflow: "implementation", RepoRef: repo, Mode: apiv1.WorkspaceRepo,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace.Path(), "implementation.txt")
	if err := os.WriteFile(file, []byte("dirty worker implementation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Remove(t.Context()); !errors.Is(err, worktree.ErrCleanupDeferred) {
		t.Fatalf("unacknowledged cleanup: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "dirty worker implementation" {
		t.Fatalf("refused upload lost source: %q %v", data, err)
	}
	allow.Store(true)
	if err := workspace.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.Path()); !os.IsNotExist(err) {
		t.Fatalf("acknowledged source not cleaned: %v", err)
	}
	entries, err := recovery.ReadInventory(t.Context(), inventory, 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("host custody missing: %v %v", entries, err)
	}
	if got := recoveryCLIGit(t, hostRepository, "show", entries[0].Record.Ref+":implementation.txt"); got != "dirty worker implementation" {
		t.Fatalf("host content = %q", got)
	}
}
