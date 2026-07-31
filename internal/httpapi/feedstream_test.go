package httpapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func feedTestStore(t *testing.T) *readmodel.Store {
	t.Helper()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func commitRun(t *testing.T, store *readmodel.Store, n int) {
	t.Helper()
	startedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
	finished := startedAt.Add(time.Minute)
	if err := store.UpsertRun(context.Background(), readmodel.Projection{Run: readmodel.RunRow{
		RunID: fmt.Sprintf("%032x", n), Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: startedAt, FinishedAt: &finished, LastSeq: uint64(n + 1),
	}}); err != nil {
		t.Fatalf("commit run %d: %v", n, err)
	}
}

// TestFeedStreamOpensWithASnapshotAtHead pins that a cursorless client starts at
// the CURRENT head, not at zero.
//
// Starting at zero would replay the entire retained feed into a client that is
// about to refetch everything anyway — on a busy instance, thousands of events
// delivered immediately before the snapshot the client actually needs.
func TestFeedStreamOpensWithASnapshotAtHead(t *testing.T) {
	store := feedTestStore(t)
	for i := 0; i < 20; i++ {
		commitRun(t, store, i)
	}

	stream := newFeedStream(store)
	defer stream.Close()

	initial, _, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	if len(initial) != 1 || initial[0].Type != "snapshot" {
		t.Fatalf("opening events = %+v, want one snapshot", initial)
	}
	head, err := readmodel.NewFeed(store).Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial[0].ID != head.String() {
		t.Errorf("snapshot cursor = %q, want the current head %q; a client starting at zero "+
			"would be sent the whole retained feed", initial[0].ID, head.String())
	}
}

// TestFeedStreamDeliversCommittedChanges is the basic liveness property.
func TestFeedStreamDeliversCommittedChanges(t *testing.T) {
	store := feedTestStore(t)
	stream := newFeedStream(store)
	defer stream.Close()

	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	commitRun(t, store, 1)
	stream.feed.Notify()

	select {
	case event := <-events:
		if event.Type != "update" {
			t.Errorf("event type = %q, want update", event.Type)
		}
		if len(event.Data.RunIDs) != 1 {
			t.Errorf("event carried %d run ids, want 1", len(event.Data.RunIDs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event delivered for a committed change")
	}
}

// TestFeedStreamNamesEachRefusalCondition pins that the three conditions reach
// the wire distinctly.
//
// A single generic stale_cursor would make a rebuilt store indistinguishable
// from a pruned feed or a client that is simply too old — and the client's
// correct response differs in each case.
func TestFeedStreamNamesEachRefusalCondition(t *testing.T) {
	store := feedTestStore(t)
	for i := 0; i < 6; i++ {
		commitRun(t, store, i)
	}
	stream := newFeedStream(store)
	defer stream.Close()

	head, err := readmodel.NewFeed(store).Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name   string
		cursor readmodel.Cursor
		want   string
	}{
		{"epoch", readmodel.Cursor{SchemaVersion: head.SchemaVersion, Epoch: "other", Seq: 1}, "epoch_changed"},
		{"schema", readmodel.Cursor{SchemaVersion: head.SchemaVersion + 5, Epoch: head.Epoch, Seq: 1}, "schema_changed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, _, err := stream.Subscribe(testCase.cursor.String())
			code, message, ok := cursorConditionStatus(err)
			if !ok {
				t.Fatalf("refusal was not a named cursor condition: %v", err)
			}
			if code != testCase.want {
				t.Errorf("code = %q, want %q", code, testCase.want)
			}
			if message == "" {
				t.Error("the refusal carries no message; a client cannot tell the user what to do")
			}
		})
	}
}

// TestFeedStreamRejectsAMalformedCursor pins that garbage is a 400-class
// refusal rather than a silent restart at head.
//
// Silently restarting would hide a client bug behind what looks like a normal
// reconnect, and the client would keep sending the same malformed value.
func TestFeedStreamRejectsAMalformedCursor(t *testing.T) {
	store := feedTestStore(t)
	stream := newFeedStream(store)
	defer stream.Close()

	_, _, _, err := stream.Subscribe("not-a-cursor")
	if !errors.Is(err, ErrInvalidEventCursor) {
		t.Errorf("malformed cursor gave %v, want ErrInvalidEventCursor", err)
	}
}

// TestFeedStreamCoalescesAPageIntoOneEvent pins the batching rule.
//
// One event carrying every affected entity, not one per change: a client
// applies a batch as a single invalidation, and splitting it would turn what
// the projector committed as one advance into N refetches.
func TestFeedStreamCoalescesAPageIntoOneEvent(t *testing.T) {
	store := feedTestStore(t)
	stream := newFeedStream(store)
	defer stream.Close()

	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	for i := 1; i <= 5; i++ {
		commitRun(t, store, i)
	}
	stream.feed.Notify()

	select {
	case event := <-events:
		if len(event.Data.RunIDs) < 2 {
			t.Errorf("a batch of 5 commits produced an event with %d run ids; the page should "+
				"coalesce rather than emit one event per change", len(event.Data.RunIDs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event delivered")
	}
}

// TestFeedStreamResumeNeedsNoInMemoryHistory pins what lets the poller go.
//
// The filesystem stream's session id is random per process, so a client could
// only resume against the process that served it. A cursor is durable: a
// brand-new stream resumes one it has never seen.
func TestFeedStreamResumeNeedsNoInMemoryHistory(t *testing.T) {
	store := feedTestStore(t)
	for i := 0; i < 3; i++ {
		commitRun(t, store, i)
	}
	head, err := readmodel.NewFeed(store).Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 3; i < 6; i++ {
		commitRun(t, store, i)
	}

	// A stream that has never seen this cursor, standing in for a different
	// process after a restart.
	fresh := newFeedStream(store)
	defer fresh.Close()
	_, events, cancel, err := fresh.Subscribe(head.String())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer cancel()

	select {
	case event := <-events:
		if len(event.Data.RunIDs) == 0 {
			t.Error("resumed stream delivered an event with no entities")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a resumed cursor delivered nothing; resume must not depend on in-memory " +
			"history held by whichever process served the original connection")
	}
}
