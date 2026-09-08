package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func stoppedStatusRootHeader(t *testing.T, root string, recordedPID int) string {
	t.Helper()
	id, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	header := fmt.Sprintf("Instance root: %q; instance ID: %q; daemon: not-running; owning PID: 0\n", canonicalStatusRoot(root), id)
	if recordedPID != 0 {
		header += fmt.Sprintf("  Recorded daemon PID: %d; recorded root: %q (recorded metadata alone is not liveness)\n", recordedPID, root)
	}
	return header
}

func TestStatusRootDistinguishesLiveOwnerFromRecordedPID(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	identity := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now().UTC(), InstanceRoot: root, Version: "test"}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(layout.SchedulerDir(), "up.lock"), &identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	live := inspectStatusRoot(layout, time.Now())
	if live.ID == "" || live.Path != canonicalStatusRoot(root) || live.OwningPID != os.Getpid() || live.DaemonState != "healthy" {
		t.Fatalf("live owner unclear: %+v", live)
	}
	release()
	// Fresh-looking derived data must not turn an abandoned root into a live one.
	if err := os.WriteFile(layout.ReadDB(), []byte("fresh but non-authoritative"), 0o644); err != nil {
		t.Fatal(err)
	}
	stopped := inspectStatusRoot(layout, time.Now())
	if stopped.OwningPID != 0 || stopped.RecordedPID != os.Getpid() || stopped.DaemonState != "not-running" || stopped.ID != live.ID {
		t.Fatalf("stale metadata became live owner: %+v", stopped)
	}
}

func TestStatusRootRejectsForeignRecordedRootAsOwner(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	identity := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now().UTC(), InstanceRoot: t.TempDir(), Version: "test"}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(layout.SchedulerDir(), "up.lock"), &identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	got := inspectStatusRoot(layout, time.Now())
	if got.OwningPID != 0 || got.DaemonState != "ownership-unverified" || got.DaemonProblem == "" {
		t.Fatalf("foreign root trusted: %+v", got)
	}
	code, stdout, _ := runArgs(t, "status", "--daemon", root)
	if code != 1 || strings.Contains(stdout, "daemon running:") {
		t.Fatalf("foreign owner reported healthy: code=%d %s", code, stdout)
	}
}

func TestStatusRootSurfacesTextJSONAndDaemon(t *testing.T) {
	root := initScheduledDemo(t)
	id, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status", root}, {"status", "--daemon", root}} {
		_, stdout, _ := runArgs(t, args...)
		if !strings.Contains(stdout, "Instance root:") || !strings.Contains(stdout, id) || !strings.Contains(stdout, canonicalStatusRoot(root)) {
			t.Fatalf("missing root identity: %s", stdout)
		}
	}
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status: %s", stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.Root == nil || got.Root.ID != id || got.Root.Path != canonicalStatusRoot(root) {
		t.Fatalf("missing JSON identity: %s", stdout)
	}
}

func TestStatusRootRejectsEmptyRecordedRootInCurrentDirectory(t *testing.T) {
	root := initScheduledDemo(t)
	t.Chdir(root)
	layout := instance.NewLayout(root)
	identity := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now().UTC(), Version: "test"}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(layout.SchedulerDir(), "up.lock"), &identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	got := inspectStatusRoot(layout, time.Now())
	// The lock decoder may reject incomplete identity before root comparison.
	// Both paths must fail closed, never manufacture ownership from cwd.
	if got.OwningPID != 0 || (got.DaemonState != "ownership-unverified" && got.DaemonState != "unknown") || got.DaemonProblem == "" {
		t.Fatalf("empty root resolved to current directory and became trusted: %+v", got)
	}
}
