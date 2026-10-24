//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

func schedulerEvent(name string) journal.Event {
	return journal.Event{Type: journal.EventRunnerAnnotation, Name: name, Runner: map[string]any{"emitKey": name}}
}

// TestInstanceStoreAdapterRoundTrip: the scheduler seam reads back exactly what
// it appended, pages past one read page, dedups a retried emit key, and keeps
// the watermark in step.
func TestInstanceStoreAdapterRoundTrip(t *testing.T) {
	backend := postgresBackend(t, postgresJournalConfig(t))
	var reported []error
	log, err := backend.OpenSchedulerLog(t.Context(), func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	var store journal.InstanceStore = log
	if seq, err := store.LastSeq(); err != nil || seq != 0 {
		t.Fatalf("empty LastSeq = %d, %v", seq, err)
	}
	const total = 1003 // more than one read page
	for i := range total {
		if err := store.Append(schedulerEvent(fmt.Sprintf("e%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Append(schedulerEvent("e0000")); err != nil { // retried emit key
		t.Fatal(err)
	}
	store.AppendBestEffort(schedulerEvent("best-effort"))
	if len(reported) != 0 {
		t.Fatalf("best-effort append reported %v", reported)
	}
	last, err := store.LastSeq()
	if err != nil || last != total+1 {
		t.Fatalf("LastSeq = %d, %v; want %d (retried key must not consume a sequence)", last, err, total+1)
	}
	all, err := store.ReadAfterSeq(0)
	if err != nil || len(all) != total+1 {
		t.Fatalf("ReadAfterSeq(0) = %d events, %v", len(all), err)
	}
	for i, event := range all {
		if event.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d; paging must be contiguous", i, event.Seq)
		}
	}
	tail, err := store.ReadAfterSeq(uint64(total - 1))
	if err != nil || len(tail) != 2 || tail[1].Name != "best-effort" {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	if _, ok, err := log.RateReset(); err != nil || ok {
		t.Fatalf("RateReset = %v, %v; the cloud log has no reset marker", ok, err)
	}
	if err := log.ResetRateLimit(time.Now()); !errors.Is(err, journalpg.ErrRateResetUnsupported) {
		t.Fatalf("ResetRateLimit = %v; the cloud must refuse loudly, not no-op", err)
	}
	if store.Dir() != "" {
		t.Fatalf("Dir = %q", store.Dir())
	}
	if err := store.Close(); err != nil || store.Close() != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
}

// TestInstanceStoreAdapterFencedAcrossTakeover: another holder takes an EXPIRED
// lease (heartbeat expired on the database clock, no voluntary release), on its
// own backend, as a successor process would. The deposed leader's adapter can no
// longer append (not even best-effort), while the successor reopens the stream,
// sees the predecessor's events, and continues the sequence gap-free.
func TestInstanceStoreAdapterFencedAcrossTakeover(t *testing.T) {
	oldConfig := postgresJournalConfig(t)
	oldBackend := postgresBackend(t, oldConfig)

	var reported []error
	old, err := oldBackend.OpenSchedulerLog(t.Context(), func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := old.Append(schedulerEvent(name)); err != nil {
			t.Fatal(err)
		}
	}

	// The fixture's own renewer keeps the old lease alive, so the takeover
	// expires the heartbeat on the database clock first, as a crashed leader's
	// would lapse, then another holder acquires.
	if _, err := postgresOwnerPool(t, oldConfig).Exec(t.Context(), `
		UPDATE control.lease_heartbeat SET expires_at=now()-interval '1 second'
		WHERE instance_id=$1 AND name='controller'`, oldConfig.InstanceID); err != nil {
		t.Fatal(err)
	}
	successorManager, err := lease.New(lease.Config{Pool: oldConfig.Pool, InstanceID: oldConfig.InstanceID, Name: "controller", Holder: "successor"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := successorManager.Acquire(t.Context())
	if err != nil {
		t.Fatalf("successor cannot take the expired lease: %v", err)
	}
	t.Cleanup(func() { _ = successorManager.Release(context.Background(), next) })
	successorBackend := postgresBackend(t, journalpg.Config{
		Pool: oldConfig.Pool, InstanceID: oldConfig.InstanceID, Lease: successorManager,
		LiveOwner: func() *lease.Handle { return next },
	})

	if err := old.Append(schedulerEvent("zombie")); err == nil {
		t.Fatal("deposed leader's adapter appended after takeover")
	}
	old.AppendBestEffort(schedulerEvent("zombie-best-effort"))
	if len(reported) != 1 {
		t.Fatalf("deposed best-effort append must be reported once, got %v", reported)
	}

	successor, err := successorBackend.OpenSchedulerLog(t.Context(), nil)
	if err != nil {
		t.Fatalf("successor cannot reopen the stream: %v", err)
	}
	if last, err := successor.LastSeq(); err != nil || last != 3 {
		t.Fatalf("successor LastSeq = %d, %v; want 3", last, err)
	}
	if err := successor.Append(schedulerEvent("d")); err != nil {
		t.Fatal(err)
	}
	events, err := successor.ReadAfterSeq(0)
	if err != nil || len(events) != 4 {
		t.Fatalf("events = %d, %v; want a, b, c, d and no zombie writes", len(events), err)
	}
	for i, name := range []string{"a", "b", "c", "d"} {
		if events[i].Name != name || events[i].Seq != uint64(i+1) {
			t.Fatalf("event %d = %s/%d", i, events[i].Name, events[i].Seq)
		}
	}
	// The deposed adapter's Close must not disturb the successor's ownership.
	_ = old.Close()
	if err := successor.Append(schedulerEvent("e")); err != nil {
		t.Fatalf("successor lost the stream when the deposed adapter closed: %v", err)
	}
}
