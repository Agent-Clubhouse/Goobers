package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// TestBootstrapConfigDirBreaksTheFirstBootDeadlock is #3314's regression pin.
//
// A brand-new instance root whose config comes from a remote workflowSource —
// the GitOps shape the product recommends — had no working first boot. `goobers
// up` validated the local config directory before any sync and failed with
// "walk /var/lib/goobers/config: no such file or directory", while `goobers
// apply`, the one-shot reconcile that would populate it, requires a live
// daemon. The component that fetches config needed the daemon, and the daemon
// needed the config it had not fetched. The only way through was hand-writing a
// zero-gaggle seed manifest, which an adopter following the docs has no way to
// know about.
func TestBootstrapConfigDirBreaksTheFirstBootDeadlock(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	cfg := &instance.Config{
		WorkflowSource: &instance.WorkflowSource{Kind: instance.WorkflowSourceKindGit},
	}

	if _, err := os.Stat(l.ConfigDir()); !os.IsNotExist(err) {
		t.Fatalf("fixture is wrong: config dir already exists (%v)", err)
	}
	if err := ensureBootstrapConfigDir(l, cfg); err != nil {
		t.Fatalf("ensureBootstrapConfigDir: %v", err)
	}

	info, err := os.Stat(l.ConfigDir())
	if err != nil {
		t.Fatalf("config directory still absent after bootstrap: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("bootstrap created something other than a directory")
	}
	// The digest walk is the call that used to fail, so assert it now succeeds
	// over the bootstrapped root rather than only that the directory exists.
	if _, err := configDirectoryDigest(l.ConfigDir()); err != nil {
		t.Fatalf("configDirectoryDigest over the bootstrapped root: %v — this is the walk that failed startup", err)
	}
}

// TestBootstrapConfigDirLeavesALocalInstanceAlone is the guard on the
// narrowing. With no remote source configured, nothing would ever populate a
// missing config directory, so startup must keep failing with its own message:
// a daemon serving an empty instance forever is worse than one that says why it
// will not start.
func TestBootstrapConfigDirLeavesALocalInstanceAlone(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  *instance.Config
	}{
		{name: "no config", cfg: nil},
		{name: "no workflow source", cfg: &instance.Config{}},
		{
			name: "non-git workflow source",
			cfg:  &instance.Config{WorkflowSource: &instance.WorkflowSource{Kind: "local"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			l := instance.NewLayout(root)

			if err := ensureBootstrapConfigDir(l, tt.cfg); err != nil {
				t.Fatalf("ensureBootstrapConfigDir: %v", err)
			}
			if _, err := os.Stat(l.ConfigDir()); !os.IsNotExist(err) {
				t.Fatalf("config directory was created for an instance with no remote source (%v); a "+
					"missing local config must still fail startup", err)
			}
		})
	}
}

// TestBootstrapConfigDirDoesNotDisturbAnExistingTree keeps an ordinary boot
// untouched: the bootstrap must be inert whenever the directory is already
// there, whatever it contains.
func TestBootstrapConfigDirDoesNotDisturbAnExistingTree(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(l.ConfigDir(), "instance-marker.yaml")
	if err := os.WriteFile(existing, []byte("kind: marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &instance.Config{
		WorkflowSource: &instance.WorkflowSource{Kind: instance.WorkflowSourceKindGit},
	}
	if err := ensureBootstrapConfigDir(l, cfg); err != nil {
		t.Fatalf("ensureBootstrapConfigDir: %v", err)
	}

	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "kind: marker\n" {
		t.Fatalf("existing config content = %q, err = %v; the bootstrap must be inert on a populated tree",
			data, err)
	}
}

// TestSchedulerSetupBootstrapsAMissingConfigDirForARemoteSource covers the call
// site rather than the helper. The unit tests above pass with the setup's call
// deleted, so this is what proves the deadlock is actually broken at startup —
// the walk that produced "walk /var/lib/goobers/config: no such file or
// directory" is inside buildSchedulerSetup, not in the helper.
func TestSchedulerSetupBootstrapsAMissingConfigDirForARemoteSource(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)

	// A first boot of a GitOps instance: instance.yaml names a remote source
	// and the config tree has not been fetched yet.
	if err := os.RemoveAll(l.ConfigDir()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	withSource := string(raw) + "\nworkflowSource:\n  kind: git\n  url: https://example.invalid/acme/config.git\n" +
		"  ref: main\n  token:\n    env: GOOBERS_TEST_WORKFLOW_SOURCE_TOKEN\n"
	if err := os.WriteFile(l.ConfigFile(), []byte(withSource), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if setup != nil {
		t.Cleanup(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = setup.Shutdown(shutdownCtx)
		})
	}
	wg.Wait()
	// The STRICT builder, which is what `goobers up` uses without
	// --skip-preflight: succeeding here is the deadlock being broken, not
	// merely a different error message.
	if err != nil {
		t.Fatalf("first boot with a remote workflow source still fails: %v — this is #3314's deadlock", err)
	}
	if _, statErr := os.Stat(l.ConfigDir()); statErr != nil {
		t.Fatalf("config directory was not bootstrapped: %v", statErr)
	}
	seed, readErr := os.ReadFile(filepath.Join(l.ConfigDir(), "manifest.yaml"))
	if readErr != nil {
		t.Fatalf("seed manifest missing: %v", readErr)
	}
	// A seed that named a gaggle could schedule work before the real config
	// arrived.
	if !strings.Contains(string(seed), "gaggles: []") {
		t.Fatalf("seed manifest = %q, want it to name no gaggle", seed)
	}
}
