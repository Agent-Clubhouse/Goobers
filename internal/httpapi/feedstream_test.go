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
	// Asserted as an INVARIANT rather than as equality against a second read.
	//
	// Head() reads through the store's read-only pool, which is a separate
	// connection: under WAL a pooled reader can observe a snapshot that predates
	// the writer's most recent commit. Comparing two independent Head() reads for
	// exact equality is racy on its own, independent of the data race this file
	// also fixed.
	//
	// What actually matters is that the client does not start at zero and get
	// sent the whole retained feed.
	cursor, err := readmodel.ParseCursor(initial[0].ID)
	if err != nil {
		t.Fatalf("snapshot cursor %q does not parse: %v", initial[0].ID, err)
	}
	state, err := store.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Epoch != state.Epoch || cursor.SchemaVersion != state.SchemaVersion {
		t.Errorf("snapshot cursor %+v does not name this store's generation (%s/%d)",
			cursor, state.Epoch, state.SchemaVersion)
	}
	if cursor.Seq == 0 {
		t.Error("the snapshot cursor is at zero, so the client will be sent the entire " +
			"retained feed before the snapshot it actually needs")
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

func TestFeedStreamCloseJoinsSubscriptionPump(t *testing.T) {
	store := feedTestStore(t)
	stream := newFeedStream(store)
	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	stream.Close()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("subscription produced an event after stream shutdown")
		}
	default:
		t.Fatal("Close returned before the subscription pump exited")
	}
	cancel()
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
// One event carrying every affected entity, not one per change: a client applies
// a batch as a single invalidation, and splitting it would turn what the
// projector committed as one advance into N refetches.
//
// # Asserted against the pure function, not through the pump
//
// The first version subscribed, committed five runs, notified, and asserted the
// delivered event carried at least two run ids. That is timing-dependent twice
// over: the pump may wake and read after only some commits are visible, and the
// feed reads through the store's read-only POOL, which under WAL can observe a
// snapshot predating the writer's latest commit. It failed on a macOS shard for
// exactly that reason.
//
// The coalescing rule itself is a pure function of a page, so it is asserted
// there. End-to-end delivery has its own test above.
func TestFeedStreamCoalescesAPageIntoOneEvent(t *testing.T) {
	page := readmodel.FeedPosition{
		Cursor: readmodel.Cursor{SchemaVersion: 4, Epoch: "e1", Seq: 9},
		Changes: []readmodel.Change{
			{Seq: 5, RunID: "run-a", Gaggle: "alpha", Workflow: "wf"},
			{Seq: 6, RunID: "run-b", Gaggle: "alpha", Workflow: "wf"},
			{Seq: 7, RunID: "run-a", Gaggle: "alpha", Workflow: "wf"},
			{Seq: 8, RunID: "run-c", Gaggle: "beta", Workflow: "other"},
			{Seq: 9, RunID: "run-c", Gaggle: "beta", Workflow: "other"},
		},
	}

	events := invalidationsFor(page)
	if len(events) != 1 {
		t.Fatalf("a five-change page produced %d events, want 1; splitting turns one "+
			"committed advance into N client refetches", len(events))
	}

	event := events[0]
	if len(event.Data.RunIDs) != 3 {
		t.Errorf("run ids = %v, want 3 distinct (run-a, run-b, run-c); repeats within a page "+
			"must collapse", event.Data.RunIDs)
	}
	if len(event.Data.Workflows) != 2 {
		t.Errorf("workflows = %v, want 2 distinct", event.Data.Workflows)
	}
	// The cursor is the LAST position in the page, so a client that applies the
	// event and reconnects resumes past all of it rather than replaying the
	// middle.
	if event.ID != page.Cursor.String() {
		t.Errorf("event cursor = %q, want the page's last position %q",
			event.ID, page.Cursor.String())
	}
}

// TestEmptyPageProducesNoEvent pins that a wakeup with nothing behind it does
// not publish. A client woken to refetch nothing is pure cost.
func TestEmptyPageProducesNoEvent(t *testing.T) {
	if events := invalidationsFor(readmodel.FeedPosition{}); len(events) != 0 {
		t.Errorf("an empty page produced %d events", len(events))
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
