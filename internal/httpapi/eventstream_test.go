package httpapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// The SSE transport, against the change feed (#1929).
//
// # What was deleted and where it went
//
// This file used to hold thirteen tests, eight of which exercised the
// filesystem poller's own mechanism: byte-offset tailing, the 5-second sweep,
// per-run state retention, the in-memory ring, and the process-scoped session
// id. That mechanism no longer exists, so those tests are not ported — they
// would be testing nothing.
//
// Their PROPERTIES, where they still apply, live in feedstream_test.go against
// the source that replaced them:
//
//	scan finds only active runs, not all history  ->  the feed is an indexed
//	                                                  seek after a cursor; a
//	                                                  quiet instance does no
//	                                                  work regardless of size
//	scan discovers a run created after startup    ->  TestFeedStreamDeliversCommittedChanges
//	sweep re-arms an idle run that resumes        ->  the projector's intake
//	                                                  drain; no sweep exists
//	expired cursor recovers current state         ->  TestFeedStreamNamesEachRefusalCondition
//	                                                  (three NAMED conditions,
//	                                                  not one generic refusal)
//	ordered, deduplicable concurrent events       ->  the change table's
//	                                                  AUTOINCREMENT and the
//	                                                  projector's serialized
//	                                                  commit loop
//
// What remains here is the transport: framing, heartbeats, shutdown, and slow
// clients. Those are properties of the handler, not of any source, so they
// survive the source swap.

func feedTestStoreAt(t *testing.T, dir string) *readmodel.Store {
	t.Helper()
	store, err := readmodel.Open(filepath.Join(dir, "read.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestDefinitionInvalidationFollowsExplicitReadModelCommit pins that a config
// reload reaches clients.
//
// The name is preserved deliberately: it is referenced by name in the Windows
// CI allowlist (.github/workflows/ci.yml), so renaming it would silently drop
// the check on that platform.
//
// The MECHANISM changed completely. The poller had an out-of-band
// PublishDefinitionsChanged that bypassed the ordered feed entirely. Deleting it
// without an equivalent would have stopped the portal noticing config reloads,
// so definitions changes now go through the same change table as everything
// else — at a definite cursor position rather than through a second channel.
func TestDefinitionInvalidationFollowsExplicitReadModelCommit(t *testing.T) {
	ctx := context.Background()
	store := feedTestStoreAt(t, t.TempDir())
	stream := newFeedStream(store)
	defer stream.Close()

	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	if err := stream.PublishDefinitionsChanged(); err != nil {
		t.Fatalf("publish definitions change: %v", err)
	}
	stream.feed.Notify()

	select {
	case event := <-events:
		// Instance-wide, not scoped: a reload can change gaggle and workflow
		// inventory, so naming entities would under-report.
		joined := strings.Join(event.Data.Models, ",")
		if !strings.Contains(joined, "instance") {
			t.Errorf("models = %v, want instance included; a config reload changes "+
				"inventory the run and workflow models do not cover", event.Data.Models)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a config reload produced no invalidation; clients would not notice new " +
			"definitions until their next ordinary refresh")
	}

	// And it is IN the ordered feed, at a real position — not out of band.
	changes, err := store.Changes(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, change := range changes {
		if change.Kind == readmodel.ChangeDefinitionsChanged {
			found = true
		}
	}
	if !found {
		t.Error("the definitions change is not in the change feed; it would be invisible to " +
			"a client resuming from a cursor")
	}
}

// TestServerShutdownClosesActiveEventStreams pins that a shutting-down server
// does not hold subscriptions open.
//
// A handler property, unchanged by the source swap: a client left hanging on a
// dead server looks identical to a healthy quiet stream, which is exactly the
// failure #1711's liveness watchdog exists to catch on the other side.
func TestServerShutdownClosesActiveEventStreams(t *testing.T) {
	store := feedTestStoreAt(t, t.TempDir())
	stream := newFeedStream(store)

	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	stream.Close()

	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close on shutdown")
	}

	// A subscription attempt after shutdown is refused rather than hanging.
	if _, _, _, err := stream.Subscribe(""); err == nil {
		t.Error("subscribing to a closed stream succeeded; the client would wait forever")
	}
	_ = events
}

// TestHeartbeatIntervalIsContractual pins that both sides agree on the number.
//
// The client arms a liveness deadline against it (#1711), so this is a
// guarantee rather than a local tuning choice: a longer server interval makes
// every client's watchdog fire on a healthy stream.
func TestHeartbeatIntervalIsContractual(t *testing.T) {
	store := feedTestStoreAt(t, t.TempDir())
	stream := newFeedStream(store)
	defer stream.Close()

	if got := stream.Heartbeat(); got != readmodel.HeartbeatInterval {
		t.Errorf("heartbeat = %s, want the contractual %s", got, readmodel.HeartbeatInterval)
	}
	if stream.WriteTimeout() <= 0 {
		t.Error("write timeout is not positive; a frame write could block forever")
	}
}

// TestCursorIsReportedForHeartbeatFraming pins that an idle client still learns
// the feed position.
func TestCursorIsReportedForHeartbeatFraming(t *testing.T) {
	ctx := context.Background()
	store := feedTestStoreAt(t, t.TempDir())
	startedAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	finished := startedAt.Add(time.Minute)
	if err := store.UpsertRun(ctx, readmodel.Projection{Run: readmodel.RunRow{
		RunID: "run-hb", Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: startedAt, FinishedAt: &finished, LastSeq: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	stream := newFeedStream(store)
	defer stream.Close()
	if cursor := stream.Cursor(); cursor == "" {
		t.Error("no cursor for heartbeat framing; an idle client cannot learn where the " +
			"feed is without waiting for an update")
	}
}
