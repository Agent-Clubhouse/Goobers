package readmodel

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestChangeRowsCommitWithTheFactTheyDescribe pins §6.2's ordering.
//
// Today the projection updates on run *finish* while the stream discovers change
// by polling the filesystem — two mechanisms with different latency and failure
// modes, so "refetch" and "the data is there" can arrive out of order. Writing
// the change row in the same transaction makes that impossible rather than
// unlikely.
func TestChangeRowsCommitWithTheFactTheyDescribe(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	identity := testIdentity()
	events := completedRunEvents()

	if err := store.UpsertRun(ctx, ProjectRun(identity, Projection{}, events)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	changes, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	c := changes[0]
	if c.Kind != ChangeRunCreated {
		t.Errorf("kind = %q, want run.created for a first projection", c.Kind)
	}
	if c.RunID != identity.RunID || c.Gaggle != identity.Gaggle {
		t.Errorf("change identity = %s/%s, want %s/%s", c.RunID, c.Gaggle, identity.RunID, identity.Gaggle)
	}

	// The row it describes must be readable at the same moment.
	row, ok, err := store.GetRun(ctx, identity.RunID)
	if err != nil || !ok {
		t.Fatalf("run not readable alongside its change row: ok=%v err=%v", ok, err)
	}
	if row.Phase != journal.PhaseCompleted {
		t.Errorf("row phase = %q, want completed", row.Phase)
	}
}

// TestChangeKindsDistinguishCreationProgressAndCompletion pins that the three
// kinds are actually distinguished.
//
// A client acts on them differently: a creation may need a list prepend while a
// progression only patches a row in place — which is what stops an arriving
// event from discarding a user's pagination (#1713).
func TestChangeKindsDistinguishCreationProgressAndCompletion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	identity := testIdentity()
	events := completedRunEvents()

	// Creation, then progress, then completion — three separate projections.
	first := ProjectRun(identity, Projection{}, events[:1])
	if err := store.UpsertRun(ctx, first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	second := ProjectRun(identity, first, events[1:4])
	if err := store.UpsertRun(ctx, second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	third := ProjectRun(identity, second, events[4:])
	if err := store.UpsertRun(ctx, third); err != nil {
		t.Fatalf("upsert third: %v", err)
	}

	changes, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	want := []ChangeKind{ChangeRunCreated, ChangeRunProgressed, ChangeRunFinished}
	if len(changes) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(changes), len(want), changes)
	}
	for i, kind := range want {
		if changes[i].Kind != kind {
			t.Errorf("change %d kind = %q, want %q", i, changes[i].Kind, kind)
		}
	}
	// Sequences must be strictly increasing, or a cursor cannot resume.
	for i := 1; i < len(changes); i++ {
		if changes[i].Seq <= changes[i-1].Seq {
			t.Errorf("change sequence did not advance: %d then %d", changes[i-1].Seq, changes[i].Seq)
		}
	}
}

// TestNoOpProjectionEmitsNoChange pins that an older projection is silent.
//
// A repair sweep re-projecting a run it has already seen must not wake every
// connected client to refetch a row that did not move. Without this, repair
// would generate change traffic proportional to history rather than to actual
// change.
func TestNoOpProjectionEmitsNoChange(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	identity := testIdentity()
	events := completedRunEvents()

	full := ProjectRun(identity, Projection{}, events)
	if err := store.UpsertRun(ctx, full); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	before, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}

	// Re-apply an OLDER projection, as a repair sweep racing live projection
	// would.
	stale := ProjectRun(identity, Projection{}, events[:2])
	if err := store.UpsertRun(ctx, stale); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	after, err := store.Changes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes after stale: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a stale projection emitted %d extra change rows; repair would generate "+
			"change traffic proportional to history rather than to actual change",
			len(after)-len(before))
	}
}

// TestCursorConditionsAreNamedAndOrdered pins §8.1's three named conditions and
// the order they are evaluated in.
//
// One generic "stale cursor" forces the client to guess what to do; today's
// stream has exactly that problem. The ORDER matters too: comparing a foreign
// epoch's sequence to this store's floor is meaningless, because the numbers
// come from different generations.
func TestCursorConditionsAreNamedAndOrdered(t *testing.T) {
	state := State{SchemaVersion: 2, Epoch: "epoch-a", MinChangeSeq: 100}

	cases := []struct {
		name   string
		cursor Cursor
		want   CursorCondition
	}{
		{"resumable", Cursor{SchemaVersion: 2, Epoch: "epoch-a", Seq: 150}, CursorOK},
		{"at the floor is resumable", Cursor{SchemaVersion: 2, Epoch: "epoch-a", Seq: 100}, CursorOK},
		{"below the floor", Cursor{SchemaVersion: 2, Epoch: "epoch-a", Seq: 99}, CursorFeedTruncated},
		{"rebuilt store", Cursor{SchemaVersion: 2, Epoch: "epoch-b", Seq: 150}, CursorEpochChanged},
		{"older schema", Cursor{SchemaVersion: 1, Epoch: "epoch-a", Seq: 150}, CursorSchemaChanged},
		// The ordering cases: a foreign epoch must be reported as such even when
		// its sequence would also look truncated, because the comparison across
		// generations is meaningless.
		{"foreign epoch below the floor is epoch_changed", Cursor{SchemaVersion: 2, Epoch: "epoch-b", Seq: 1}, CursorEpochChanged},
		{"older schema and foreign epoch is schema_changed", Cursor{SchemaVersion: 1, Epoch: "epoch-b", Seq: 1}, CursorSchemaChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EvaluateCursor(tc.cursor, state); got != tc.want {
				t.Errorf("EvaluateCursor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCursorRoundTrips pins the wire form.
func TestCursorRoundTrips(t *testing.T) {
	original := Cursor{SchemaVersion: 2, Epoch: "0123456789abcdef", Seq: 918342}
	parsed, err := ParseCursor(original.String())
	if err != nil {
		t.Fatalf("parse %q: %v", original.String(), err)
	}
	if parsed != original {
		t.Errorf("round trip = %+v, want %+v", parsed, original)
	}
	for _, bad := range []string{"", "1", "1:epoch", "x:epoch:1", "1:epoch:x", "1::1"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) accepted a malformed cursor", bad)
		}
	}
}

// TestPruneAdvancesTheFloorInTheSameTransaction pins §4.2's retention rule.
//
// The floor must be a persisted fact rather than an implication: `cursor.seq <
// min_change_seq` is an exact condition, and it is only exact if the recorded
// floor can never disagree with what is actually in the table.
func TestPruneAdvancesTheFloorInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	identity := testIdentity()
	events := completedRunEvents()
	prev := Projection{}
	for i := range events {
		prev = ProjectRun(identity, prev, events[i:i+1])
		if err := store.UpsertRun(ctx, prev); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	all, err := store.Changes(ctx, 0, 100)
	if err != nil || len(all) < 3 {
		t.Fatalf("expected several changes, got %d (err %v)", len(all), err)
	}

	keepFrom := all[len(all)-1].Seq
	removed, err := store.PruneChanges(ctx, keepFrom)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed == 0 {
		t.Fatal("prune removed nothing")
	}

	state, err := store.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.MinChangeSeq != keepFrom {
		t.Errorf("min_change_seq = %d after pruning below %d; the floor and the table disagree, "+
			"so feed_truncated is no longer an exact condition", state.MinChangeSeq, keepFrom)
	}
	// And a cursor below the new floor must now be reported as truncated.
	cursor := Cursor{SchemaVersion: state.SchemaVersion, Epoch: state.Epoch, Seq: keepFrom - 1}
	if got := EvaluateCursor(cursor, state); got != CursorFeedTruncated {
		t.Errorf("cursor below the floor evaluated to %q, want feed_truncated", got)
	}

	// The floor only advances: a lower prune must not lower it.
	if _, err := store.PruneChanges(ctx, 1); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	state, err = store.State(ctx)
	if err != nil {
		t.Fatalf("state after second prune: %v", err)
	}
	if state.MinChangeSeq != keepFrom {
		t.Errorf("min_change_seq regressed to %d; already-deleted rows would be declared resumable",
			state.MinChangeSeq)
	}
}
