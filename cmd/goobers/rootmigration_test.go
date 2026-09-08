package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestRootMigrationPreservesIdentityAndDistinguishesAuthority(t *testing.T) {
	source := initScheduledDemo(t)
	destination := t.TempDir()
	if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	id, err := instance.ReadRootIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	if copied, err := instance.ReadRootIdentity(destination); err != nil || copied != id {
		t.Fatalf("migration changed identity: %q -> %q: %v", id, copied, err)
	}
	// The abandoned source looks fresher than the destination. Neither file
	// contains a real read model; discovery must use daemon ownership, not
	// interpret these bytes or rank their modification times as authority.
	for _, root := range []string{source, destination} {
		path := instance.NewLayout(root).ReadDB()
		if err := os.WriteFile(path, []byte("not daemon authority"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		if root == destination {
			at = at.Add(-24 * time.Hour)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	owner := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now().UTC(), InstanceRoot: destination, Version: "migration-test"}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(instance.NewLayout(destination).SchedulerDir(), "up.lock"), &owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	code, stdout, stderr := runArgs(t, "roots", "decommission", "--reason=migrated", source)
	if code != 0 {
		t.Fatalf("decommission source: %d %s %s", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "roots", "discover", "--json", source, destination)
	if code != 0 {
		t.Fatalf("discover migration: %d %s %s", code, stdout, stderr)
	}
	var result rootsDiscoveryOutput
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	assertMigratedRoots(t, result, source, destination, id)
	var banner bytes.Buffer
	if err := prepareManualRoot(instance.NewLayout(destination), &banner); err != nil {
		t.Fatalf("source decommission blocked destination: %v", err)
	}
	if banner.String() != manualServiceRootHeader(t, destination) {
		t.Fatalf("destination mutation target ambiguous: %s", banner.String())
	}
	code, _, stderr = runArgs(t, "up", source)
	if code != 2 || !strings.Contains(stderr, "historical root; do not use") {
		t.Fatalf("abandoned source could restart: %d %s", code, stderr)
	}
}

func assertMigratedRoots(t *testing.T, result rootsDiscoveryOutput, source, destination, id string) {
	t.Helper()
	if result.Partial || len(result.Roots) != 2 {
		t.Fatalf("migration discovery incomplete: %+v", result)
	}
	byPath := make(map[string]discoveredRoot)
	for _, root := range result.Roots {
		if root.ID != id || root.IdentityProblem != "" || root.LifecycleProblem != "" {
			t.Fatalf("migration identity lost: %+v", root)
		}
		byPath[root.Path] = root
	}
	old, current := byPath[canonicalStatusRoot(source)], byPath[canonicalStatusRoot(destination)]
	if old.DecommissionedAt == nil || old.DecommissionReason != "migrated" || old.OwningPID != 0 || old.DaemonState != "not-running" {
		t.Fatalf("historical source appears authoritative: %+v", old)
	}
	if current.DecommissionedAt != nil || current.OwningPID != os.Getpid() || current.DaemonState != "healthy" {
		t.Fatalf("destination ownership ambiguous: %+v", current)
	}
	if old.ReadModelModifiedAt == nil || current.ReadModelModifiedAt == nil || !old.ReadModelModifiedAt.After(*current.ReadModelModifiedAt) {
		t.Fatal("test did not exercise misleading source database freshness")
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "copied identity") {
		t.Fatalf("expected copied identity warning, not duplicate active scope: %v", result.Warnings)
	}
}
