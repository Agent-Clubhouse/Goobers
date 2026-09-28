package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

func TestStartupConfiguration(t *testing.T) {
	base := startupConfig{Rounds: 20, Prefill: "empty", Index: "cold", Endpoint: "healthy", Settle: time.Second}
	for _, name := range []string{"empty", "half", "near-cap", "cap", "tiny-files", "legacy"} {
		c := base
		c.Prefill = name
		if err := c.validate("startup"); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*startupConfig){
		func(c *startupConfig) { c.Rounds = 0 },
		func(c *startupConfig) { c.Rounds = 101 },
		func(c *startupConfig) { c.Prefill = "unknown" },
		func(c *startupConfig) { c.Index = "OS-cold" },
		func(c *startupConfig) { c.Endpoint = "external" },
		func(c *startupConfig) { c.Settle = -time.Second },
		func(c *startupConfig) { c.Settle = 61 * time.Second },
	} {
		c := base
		change(&c)
		if c.validate("startup") == nil {
			t.Fatalf("invalid configuration accepted: %+v", c)
		}
	}
}

func TestStartupReadinessRequiresBothRoutes(t *testing.T) {
	for _, failing := range []string{"", "/readyz", "/api/v1/instance"} {
		t.Run(failing, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == failing {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			if got := startupReady(server.URL); got != (failing == "") {
				t.Fatalf("ready=%v failing=%q", got, failing)
			}
		})
	}
}

func TestStartupWarmRequiresCompleteAccounting(t *testing.T) {
	state := startupOccupancy{Manifest: true, Files: 2, Bytes: 100}
	stats := telemetry.AzureReplayStats{AccountingReady: true, PendingFiles: 2, PendingBytes: 100}
	if err := validateStartupWarm(state, stats); err != nil {
		t.Fatal(err)
	}
	for _, incomplete := range []telemetry.AzureReplayStats{
		{AccountingReady: true}, // Migrated schema is not reconciled accounting.
		{AccountingReady: false, PendingFiles: 2, PendingBytes: 100},
		{AccountingReady: true, PendingFiles: 1, PendingBytes: 100},
		{AccountingReady: true, PendingFiles: 2, PendingBytes: 99},
	} {
		if validateStartupWarm(state, incomplete) == nil {
			t.Fatalf("incomplete accounting accepted: %+v", incomplete)
		}
	}
	state.Manifest = false
	if validateStartupWarm(state, stats) == nil {
		t.Fatal("missing manifest accepted")
	}
}

func TestStartupSeedCleanupIsScoped(t *testing.T) {
	previous := out
	out = t.TempDir()
	t.Cleanup(func() { out = previous })
	root := filepath.Join(out, "startup-owned")
	seed := filepath.Join(spool(root), "journal", "00000000000000000000-seed.ndjson")
	other := filepath.Join(spool(root), "journal", "keep.ndjson")
	write(seed, "synthetic seed\n")
	write(other, "keep\n")
	if err := visitStartupSeeds(root, true); err == nil {
		t.Fatal("cleanup without marker succeeded")
	}
	write(filepath.Join(root, ".startup-fixture"), "synthetic-startup-seeds-v1\n")
	if err := visitStartupSeeds(root, false); err != nil {
		t.Fatal(err)
	}
	before, err := startupDiskState(root)
	if err != nil || before.Files != 2 || before.Bytes != 20 || before.Manifest {
		t.Fatalf("incorrect pre-cleanup occupancy: %+v %v", before, err)
	}
	if err := visitStartupSeeds(root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(seed); !os.IsNotExist(err) {
		t.Fatalf("seed not removed: %v", err)
	}
	if body, err := os.ReadFile(other); err != nil || string(body) != "keep\n" {
		t.Fatalf("unrelated file changed: %q %v", body, err)
	}
	if err := visitStartupSeeds(filepath.Dir(out), true); err == nil {
		t.Fatal("outside output path accepted")
	}
}

func TestStartupSeedCleanupRejectsSymlink(t *testing.T) {
	previous := out
	out = t.TempDir()
	t.Cleanup(func() { out = previous })
	root := filepath.Join(out, "startup-owned")
	write(filepath.Join(root, ".startup-fixture"), "synthetic-startup-seeds-v1\n")
	journal := filepath.Join(spool(root), "journal")
	if err := os.MkdirAll(journal, 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "keep.ndjson")
	write(external, "untouched")
	if err := os.Symlink(external, filepath.Join(journal, "00000000000000000000-seed.ndjson")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := visitStartupSeeds(root, true); err == nil {
		t.Fatal("symlink seed accepted")
	}
	if _, err := startupDiskState(root); err == nil {
		t.Fatal("symlink occupancy accepted")
	}
	if body, err := os.ReadFile(external); err != nil || string(body) != "untouched" {
		t.Fatalf("external target changed: %q %v", body, err)
	}
}
