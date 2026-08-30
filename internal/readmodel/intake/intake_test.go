package intake

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open intake: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestConcurrentFirstOpenSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store, err := Open(path)
			if err != nil {
				errs <- fmt.Errorf("open %d: %w", i, err)
				return
			}
			defer func() { _ = store.Close() }()
			if err := store.Observed(context.Background(), fmt.Sprintf("run-%d", i), uint64(i+1)); err != nil {
				errs <- fmt.Errorf("observe %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestAdvancingRunCancelsStaleRemovalIntent is #1922's first named fixture:
// "a run advances while removal intent is pending → not deleted."
//
// This is the one place a writer overrides retention, and the direction matters.
// If retention crashes between recording intent and unlinking, and the run then
// advances or is resumed, a higher source_seq is PROOF the intent is stale — a
// removed journal cannot produce new events. Without clearing the flag, the
// projector takes the removal branch and deletes a run that is actively
// executing.
func TestAdvancingRunCancelsStaleRemovalIntent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const runID = "run-advancing"

	if err := store.Observed(ctx, runID, 10); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if err := store.Removing(ctx, runID); err != nil {
		t.Fatalf("mark removing: %v", err)
	}
	marker, ok, err := store.Get(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if !marker.Removing {
		t.Fatal("removal intent was not recorded; the rest of this test proves nothing")
	}

	// The run advances. A removed journal cannot do this.
	if err := store.Observed(ctx, runID, 25); err != nil {
		t.Fatalf("observe after intent: %v", err)
	}

	marker, ok, err = store.Get(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("get after advance: ok=%v err=%v", ok, err)
	}
	if marker.Removing {
		t.Error("a run that advanced to seq 25 still carries removal intent; the projector " +
			"would take the removal branch and delete a LIVE run's rows")
	}
	if marker.SourceSeq != 25 {
		t.Errorf("source sequence = %d, want 25", marker.SourceSeq)
	}
}

// TestEqualSequenceAlsoCancelsRemovalIntent covers the boundary the guard is
// written with `>=` rather than `>` for.
//
// Retention can mark a run that has already stopped advancing. A writer that
// then re-reports the same position — a resume that produced no new events yet,
// or a redundant observation — is still evidence the run is being handled. With
// a strict `>`, the marker would keep its removal flag and the run would be
// deleted anyway.
func TestEqualSequenceAlsoCancelsRemovalIntent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const runID = "run-equal"

	if err := store.Observed(ctx, runID, 7); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if err := store.Removing(ctx, runID); err != nil {
		t.Fatalf("mark removing: %v", err)
	}
	if err := store.Observed(ctx, runID, 7); err != nil {
		t.Fatalf("re-observe: %v", err)
	}

	marker, _, err := store.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if marker.Removing {
		t.Error("a re-observation at the same sequence left removal intent standing")
	}
}

// TestStaleObservationCannotRewindTheWatermark pins the other half of the same
// guard.
//
// A slow process holding an old sequence must not lower the watermark — doing so
// would make the projector re-emit work it has already acknowledged, and worse,
// would let an ack at the lower sequence delete a marker that represents newer
// work.
func TestStaleObservationCannotRewindTheWatermark(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const runID = "run-rewind"

	if err := store.Observed(ctx, runID, 100); err != nil {
		t.Fatalf("observe 100: %v", err)
	}
	if err := store.Observed(ctx, runID, 40); err != nil {
		t.Fatalf("observe 40: %v", err)
	}

	marker, _, err := store.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if marker.SourceSeq != 100 {
		t.Errorf("a stale observation rewound the watermark to %d; work already reported "+
			"would be acknowledged away", marker.SourceSeq)
	}
}

// TestAckLeavesAMarkerThatAdvancedDuringProjection is the protocol's central
// property.
//
// The acknowledgement cannot be inside the projection transaction — read.db and
// intake.db are separate files, and SQLite's WAL gives no atomic commit across
// databases. So work that arrives WHILE a projection is in flight must not be
// acknowledged by it. The guard is `source_seq <= projectedSeq`: a newer append
// left a higher sequence, the delete no-ops, and the projector revisits.
//
// Without it, an append landing between the read and the ack is lost until the
// repair sweep notices — which can be a full cycle later.
func TestAckLeavesAMarkerThatAdvancedDuringProjection(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const runID = "run-raced"

	// The projector observes seq 10 and begins projecting.
	if err := store.Observed(ctx, runID, 10); err != nil {
		t.Fatalf("observe: %v", err)
	}
	// The run appends more while that projection is in flight.
	if err := store.Observed(ctx, runID, 30); err != nil {
		t.Fatalf("observe during projection: %v", err)
	}
	// The projector finishes its work for seq 10 and acknowledges.
	acked, err := store.Ack(ctx, runID, 10)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked {
		t.Error("the ack consumed a marker representing NEWER work; everything between " +
			"seq 10 and 30 would be invisible until the repair sweep")
	}

	marker, ok, err := store.Get(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("marker must survive: ok=%v err=%v", ok, err)
	}
	if marker.SourceSeq != 30 {
		t.Errorf("surviving marker is at %d, want 30", marker.SourceSeq)
	}

	// Projecting through 30 does acknowledge it.
	acked, err = store.Ack(ctx, runID, 30)
	if err != nil {
		t.Fatalf("ack at 30: %v", err)
	}
	if !acked {
		t.Error("projecting through the reported sequence did not acknowledge the marker")
	}
	if _, ok, _ := store.Get(ctx, runID); ok {
		t.Error("marker survived an acknowledgement that covered it")
	}
}

// TestOrdinaryAckCannotConsumeARemovalMarker pins the `removing = 0` half of the
// guard.
//
// A run can be projected normally while retention has already recorded intent.
// If that ordinary acknowledgement cleared the marker, the subsequent unlink
// would leave a projected row with no journal behind it — discoverable only by
// the bidirectional repair sweep, and until then a run the portal shows but
// cannot open.
func TestOrdinaryAckCannotConsumeARemovalMarker(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const runID = "run-removing"

	if err := store.Observed(ctx, runID, 5); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if err := store.Removing(ctx, runID); err != nil {
		t.Fatalf("mark removing: %v", err)
	}

	acked, err := store.Ack(ctx, runID, 999)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked {
		t.Error("an ordinary acknowledgement consumed a REMOVAL marker; the unlink would " +
			"then leave a projected row with no journal")
	}

	marker, ok, err := store.Get(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("removal marker must survive: ok=%v err=%v", ok, err)
	}
	if !marker.Removing {
		t.Error("the marker lost its removal intent")
	}

	// The confirm step is the one that clears it.
	if err := store.AckRemoval(ctx, runID); err != nil {
		t.Fatalf("ack removal: %v", err)
	}
	if _, ok, _ := store.Get(ctx, runID); ok {
		t.Error("the removal marker survived its confirmation")
	}
}

// TestRetentionCrashBetweenIntentAndUnlinkResolvesOnTheNextPass is #1922's
// second named fixture.
//
// Recording intent BEFORE the unlink is what makes an interrupted retention pass
// recoverable. Whichever way the crash falls, the next pass sees enough to
// finish: the marker is still pending and still flagged, so the projector takes
// the removal branch.
func TestRetentionCrashBetweenIntentAndUnlinkResolvesOnTheNextPass(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)

	// Pass one: record intent, then "crash" — close without unlinking.
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Observed(ctx, "run-crashed", 12); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if err := first.Removing(ctx, "run-crashed"); err != nil {
		t.Fatalf("mark removing: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Pass two: a fresh process sees the pending intent.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	pending, err := second.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	var found bool
	for _, marker := range pending {
		if marker.RunID == "run-crashed" && marker.Removing {
			found = true
		}
	}
	if !found {
		t.Error("removal intent did not survive the crash; the journal would be unlinked " +
			"with no record that anyone meant to, leaving a projected row orphaned")
	}
}

// TestExternalWriterAcrossAnEpochSwapLosesNothing is #1922's third named
// fixture.
//
// intake.db is never rebuilt. A process that opened it before an epoch swap and
// keeps writing afterwards is writing to the same file the projector will read —
// which is the whole reason intake cannot live inside read.db, where the swap
// would replace the inode underneath it.
func TestExternalWriterAcrossAnEpochSwapLosesNothing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)

	// An external process (say `goobers run`) opens intake and holds it.
	external, err := Open(path)
	if err != nil {
		t.Fatalf("open external: %v", err)
	}
	t.Cleanup(func() { _ = external.Close() })
	if err := external.Observed(ctx, "run-before", 1); err != nil {
		t.Fatalf("observe before: %v", err)
	}

	// The daemon rebuilds read.db into a new epoch. intake.db is untouched — this
	// test asserts that by opening a second handle, which is what a rebuilt
	// projector does, and checking both see each other's writes.
	projector, err := Open(path)
	if err != nil {
		t.Fatalf("open projector: %v", err)
	}
	t.Cleanup(func() { _ = projector.Close() })

	// The external process, which never noticed the swap, keeps writing.
	if err := external.Observed(ctx, "run-after", 2); err != nil {
		t.Fatalf("observe after: %v", err)
	}

	pending, err := projector.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	seen := make(map[string]bool, len(pending))
	for _, marker := range pending {
		seen[marker.RunID] = true
	}
	for _, want := range []string{"run-before", "run-after"} {
		if !seen[want] {
			t.Errorf("the post-rebuild projector cannot see %s; an external writer's "+
				"watermarks were lost across the epoch swap", want)
		}
	}
}

// TestPendingIsOldestFirst pins the drain order.
//
// Oldest-first matters under a burst: a newest-first drain lets a busy instance
// starve an early marker indefinitely, so a single quiet run could wait behind
// an unbounded stream of active ones and only be discovered by the sweep.
func TestPendingIsOldestFirst(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	for _, id := range []string{"a", "b", "c", "d"} {
		if err := store.Observed(ctx, id, 1); err != nil {
			t.Fatalf("observe %s: %v", id, err)
		}
	}
	pending, err := store.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 4 {
		t.Fatalf("got %d markers, want 4", len(pending))
	}
	for i := 1; i < len(pending); i++ {
		if pending[i].ObservedAt.Before(pending[i-1].ObservedAt) {
			t.Errorf("marker %d was observed before marker %d; the drain is not oldest-first "+
				"and a burst could starve an early marker", i, i-1)
		}
	}
}

// TestPendingIsBounded pins that a pathological backlog is drained across
// passes rather than read in one unbounded query.
func TestPendingIsBounded(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	for i := 0; i < 40; i++ {
		if err := store.Observed(ctx, string(rune('a'+i%26))+string(rune('a'+i/26)), 1); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}
	pending, err := store.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) > 10 {
		t.Errorf("a drain with limit 10 returned %d markers", len(pending))
	}
}

// TestAbsentMarkerIsAnAnswerNotAnError: a run with no pending intake is the
// ordinary case — every run that has been fully projected. Reporting it as an
// error would make the common path noisy.
func TestAbsentMarkerIsAnAnswerNotAnError(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	_, ok, err := store.Get(ctx, "never-seen")
	if err != nil {
		t.Errorf("reading an absent marker returned an error: %v", err)
	}
	if ok {
		t.Error("the store claims to hold a marker that was never written")
	}
}

func TestFileURIIsAbsoluteSoSQLiteCanOpenIt(t *testing.T) {
	// Regression: on Windows filepath.ToSlash yields "C:/dir/intake.db", whose
	// first segment contains a colon. url.URL.String then prefixes "./" so the
	// segment cannot be read as a scheme, producing "file:./C:/dir/intake.db" —
	// a relative URI SQLite rejects with SQLITE_CANTOPEN. POSIX paths are
	// already rooted, which is why this only ever failed on Windows.
	uri := fileURI(filepath.Join(t.TempDir(), FileName))

	if !strings.HasPrefix(uri, "file:///") {
		t.Errorf("fileURI = %q, want a rooted file:/// URI", uri)
	}
	if strings.Contains(uri, "./") {
		t.Errorf("fileURI = %q, want no relative segment", uri)
	}
}

func TestOpenAcceptsAHostAbsolutePath(t *testing.T) {
	// Open must work against a real absolute path on every supported OS, not
	// only where the path happens to start with a slash.
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open with an absolute host path: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
}

// TestOldestPendingReportsBacklogAge pins the age half of the freshness
// surface: a count alone cannot distinguish a projector mid-pass from one that
// has stopped.
func TestOldestPendingReportsBacklogAge(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if _, found, err := store.OldestPending(ctx); err != nil {
		t.Fatalf("oldest pending on an empty store: %v", err)
	} else if found {
		t.Error("an empty intake store reported a pending observation; a zero time reads " +
			"as infinitely stale rather than as no backlog")
	}

	if err := store.Observed(ctx, "run-a", 1); err != nil {
		t.Fatal(err)
	}
	first, found, err := store.OldestPending(ctx)
	if err != nil || !found {
		t.Fatalf("oldest pending after one observation: %v, found=%v", err, found)
	}
	if err := store.Observed(ctx, "run-b", 1); err != nil {
		t.Fatal(err)
	}
	oldest, found, err := store.OldestPending(ctx)
	if err != nil || !found {
		t.Fatalf("oldest pending after two observations: %v, found=%v", err, found)
	}
	if !oldest.Equal(first) {
		t.Errorf("oldest pending = %s, want the first observation %s", oldest, first)
	}

	if _, err := store.Ack(ctx, "run-a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(ctx, "run-b", 1); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.OldestPending(ctx); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("a drained intake store still reports a backlog")
	}
}
