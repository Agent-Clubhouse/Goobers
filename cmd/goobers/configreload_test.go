package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestUpReloadsValidConfigAndRejectsInvalidEdit(t *testing.T) {
	previousReloadInterval := configReloadInterval
	previousDelegationInterval := delegationSweepInterval
	configReloadInterval = 20 * time.Millisecond
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		configReloadInterval = previousReloadInterval
		delegationSweepInterval = previousDelegationInterval
	})

	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runUpContext(ctx, []string{"--quiet", "--watch-config", root}, started, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-daemonDone:
			if code != 0 {
				t.Errorf("daemon exit code = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-daemonDone:
		t.Fatalf("daemon exited before startup with code %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	initialHealth := readDaemonHealth(t, address)
	if initialHealth.Instance.Name != "example" || initialHealth.Instance.Environment != apiv1.EnvironmentDev {
		t.Fatalf("initial instance = %+v", initialHealth.Instance)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	reloadedManifest := strings.Replace(
		string(manifest),
		"    name: example\n    environment: dev",
		"    name: reloaded-example\n    environment: staging",
		1,
	)
	if reloadedManifest == string(manifest) {
		t.Fatal("manifest identity fixture not found")
	}
	if err := os.WriteFile(manifestPath, []byte(reloadedManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	oldDigest, oldOK := reloaded.Runner["oldDigest"].(string)
	newDigest, newOK := reloaded.Runner["newDigest"].(string)
	if !oldOK || !newOK || oldDigest == "" || newDigest == "" || oldDigest == newDigest {
		t.Fatalf("config.reloaded digests = %+v, want distinct old/new digests", reloaded.Runner)
	}
	reloadedHealth := waitForDaemonHealth(t, address, "reloaded-example", apiv1.EnvironmentStaging)
	if !reloadedHealth.Freshness.DefinitionsLoadedAt.After(initialHealth.Freshness.DefinitionsLoadedAt) {
		t.Fatalf(
			"definitionsLoadedAt = %s, want after startup value %s",
			reloadedHealth.Freshness.DefinitionsLoadedAt,
			initialHealth.Freshness.DefinitionsLoadedAt,
		)
	}

	reloadedWorkflow := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: reloaded-implement", 1)
	if err := os.WriteFile(workflowPath, []byte(reloadedWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 2)

	code, stdout, stderr := runArgs(t, "run", "reloaded-implement", root)
	if code != 0 {
		t.Fatalf("run reloaded workflow: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if err := os.WriteFile(workflowPath, []byte("kind: Workflow\nmetadata: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rejected := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	if rejected.Error == nil || rejected.Error.Code != "config_reload_rejected" || rejected.Error.Message == "" {
		t.Fatalf("config.reload.rejected error = %+v", rejected.Error)
	}

	code, stdout, stderr = runArgs(t, "run", "reloaded-implement", root)
	if code != 0 {
		t.Fatalf("last-known-good workflow unavailable after rejected edit: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func readDaemonHealth(t *testing.T, address string) readservice.Health {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://" + address + httpapi.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("health status = %d", response.StatusCode)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return health
}

func waitForDaemonHealth(t *testing.T, address, name string, environment apiv1.Environment) readservice.Health {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		health := readDaemonHealth(t, address)
		if health.Instance.Name == name && health.Instance.Environment == environment {
			return health
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for health identity %s/%s", name, environment)
	return readservice.Health{}
}

func TestBuildSchedulerSetupRejectsConfigChangedDuringStartup(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")

	previousLoader := loadConfigDirectory
	loadConfigDirectory = func(dir string) (*instance.ConfigSet, *validate.Report, error) {
		set, report, err := instance.LoadConfigDir(dir)
		if err != nil {
			return set, report, err
		}
		changed := strings.Replace(deterministicWorkflowYAML, "name: default-implement", "name: changed-during-startup", 1)
		if err := os.WriteFile(workflowPath, []byte(changed), 0o644); err != nil {
			return nil, report, err
		}
		return set, report, nil
	}
	t.Cleanup(func() { loadConfigDirectory = previousLoader })

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if setup != nil {
		setup.Shutdown(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "config directory changed during daemon setup") {
		t.Fatalf("buildSchedulerSetup error = %v, want changed-during-setup refusal", err)
	}
}

func waitForConfigEvent(t *testing.T, schedulerDir string, eventType journal.EventType, count int) journal.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := journal.ReadInstanceLog(schedulerDir)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, event := range events {
			if event.Type != eventType {
				continue
			}
			seen++
			if seen == count {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s event %d", eventType, count)
	return journal.Event{}
}

func TestConfigDirectoryDigestOnlyTracksLoadedConfigAndAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}

	// Non-config churn must NOT move the digest: a README, editor swap/backup
	// files, and a .git worktree are all outside the loader's surface, so
	// touching them must not trigger a reload or a false rejection.
	noise := map[string]string{
		"README.md":          "# docs\n",
		".manifest.yaml.swp": "vim-swap-garbage",
		"manifest.yaml~":     "editor backup",
		"4913":               "vim probe file",
		".git/index":         "git internals",
		".git/HEAD":          "ref: refs/heads/main\n",
		"config.json":        "{}",
	}
	for name, content := range noise {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got != baseline {
		t.Fatalf("non-config churn changed digest: got %s, want %s", got, baseline)
	}

	asset := filepath.Join(root, "gaggles", "example", "goobers", "coder", "assets", ".hidden", "reference.txt")
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("static reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	withAsset, err := configDirectoryDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if withAsset == baseline {
		t.Fatalf("asset addition did not change digest: %s", withAsset)
	}

	// A real change to a loaded config file MUST move the digest.
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := configDirectoryDigest(root); err != nil {
		t.Fatal(err)
	} else if got == withAsset {
		t.Fatalf("config edit did not change digest: %s", got)
	}
}

func TestConfigDirectoryDigestRejectsUnsafeAssets(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"symlink": func(t *testing.T, assets string) {
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(assets, "reference")); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
		},
		"special file": func(t *testing.T, assets string) {
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(filepath.Join(assets, "stream"), 0o600); err != nil {
				t.Skipf("FIFO unsupported: %v", err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: A\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(root, "gaggles", "example", "goobers", "coder", "assets")
			if err := os.MkdirAll(filepath.Dir(assets), 0o755); err != nil {
				t.Fatal(err)
			}
			setup(t, assets)
			if _, err := configDirectoryDigest(root); err == nil {
				t.Fatal("configDirectoryDigest accepted unsafe assets")
			}
		})
	}
}

func TestConfigDirectoryDigestSkipsVanishedFileWithoutError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("kind: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink models a config path that WalkDir enumerates but that
	// has vanished by the time it is read (an atomic write-then-rename mid-poll).
	// The digest must skip it and succeed, never returning an error the poll
	// loop would journal as config.reload.rejected.
	if err := os.Symlink(filepath.Join(root, "gone.yaml"), filepath.Join(root, "pending.yaml")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := configDirectoryDigest(root); err != nil {
		t.Fatalf("vanished config file surfaced as error: %v", err)
	}
}
