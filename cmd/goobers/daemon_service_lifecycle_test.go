package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestDaemonLifecycleJournalsDirtyRestartCause(t *testing.T) {
	schedulerDir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	prior := &daemonIdentity{
		PID:          41,
		StartedAt:    time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	current := &daemonIdentity{
		PID:          42,
		StartedAt:    time.Date(2026, 7, 23, 12, 0, 5, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	if err := journalDaemonStart(log, priorDaemonLock{exists: true, identity: prior}, current); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != journal.EventDaemonDirtyRestart || events[0].Reason != dirtyRestartCause {
		t.Fatalf("dirty restart event = %+v", events[0])
	}
	if events[0].Runner["pid"] != float64(41) {
		t.Fatalf("previous pid = %#v", events[0].Runner["pid"])
	}
	if events[1].Type != journal.EventDaemonStarted || events[1].Runner["pid"] != float64(42) {
		t.Fatalf("start event = %+v", events[1])
	}
}

func TestDaemonLifecycleCleanShutdownSuppressesDirtyRestart(t *testing.T) {
	schedulerDir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	first := &daemonIdentity{
		PID:          41,
		StartedAt:    time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	second := &daemonIdentity{
		PID:          42,
		StartedAt:    time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	if err := journalDaemonStart(log, priorDaemonLock{}, first); err != nil {
		t.Fatal(err)
	}
	if err := journalDaemonCleanShutdown(log, first); err != nil {
		t.Fatal(err)
	}
	if err := journalDaemonStart(log, priorDaemonLock{exists: true, identity: first}, second); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var dirty int
	for _, event := range events {
		if event.Type == journal.EventDaemonDirtyRestart {
			dirty++
		}
	}

	if dirty != 0 {
		t.Fatalf("dirty restart events = %d; events = %+v", dirty, events)
	}
	if got := events[len(events)-1].Type; got != journal.EventDaemonStarted {
		t.Fatalf("last event = %s", got)
	}
}

func TestDaemonLifecycleDetectsLockReplacedAfterCleanShutdown(t *testing.T) {
	schedulerDir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	clean := &daemonIdentity{
		PID:          41,
		StartedAt:    time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	crashedBeforeStartEvent := &daemonIdentity{
		PID:          42,
		StartedAt:    time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	restarted := &daemonIdentity{
		PID:          43,
		StartedAt:    time.Date(2026, 7, 23, 13, 0, 5, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	if err := journalDaemonStart(log, priorDaemonLock{}, clean); err != nil {
		t.Fatal(err)
	}
	if err := journalDaemonCleanShutdown(log, clean); err != nil {
		t.Fatal(err)
	}
	if err := journalDaemonStart(log, priorDaemonLock{exists: true, identity: crashedBeforeStartEvent}, restarted); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-2]; got.Type != journal.EventDaemonDirtyRestart || got.Reason != dirtyRestartCause {
		t.Fatalf("dirty restart event = %+v", got)
	}
}

func TestDaemonLifecycleIgnoresFreshManualLock(t *testing.T) {
	schedulerDir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	current := &daemonIdentity{
		PID:          42,
		StartedAt:    time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC),
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	if err := journalDaemonStart(log, priorDaemonLock{exists: true}, current); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != journal.EventDaemonStarted {
		t.Fatalf("events = %+v", events)
	}
}

func TestReadPriorDaemonLockPreservesCrashIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	startedAt := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	identity := &daemonIdentity{
		PID:          41,
		StartedAt:    startedAt,
		InstanceRoot: "/srv/goobers",
		Version:      "v1.0.0",
	}
	release, err := acquireInstanceLockWithIdentity(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	release()

	prior, err := readPriorDaemonLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !prior.exists || prior.identity == nil || prior.identity.PID != 41 || !prior.identity.StartedAt.Equal(startedAt) {
		t.Fatalf("prior lock = %+v", prior)
	}
}

func TestReadPriorDaemonLockToleratesTornIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	if err := os.WriteFile(path, []byte(`{"pid":`), 0o644); err != nil {
		t.Fatal(err)
	}
	prior, err := readPriorDaemonLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !prior.exists || prior.identity != nil || prior.readErr == "" {
		t.Fatalf("prior lock = %+v", prior)
	}
}
