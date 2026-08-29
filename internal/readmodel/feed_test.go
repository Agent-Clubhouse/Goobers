package readmodel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func seedChange(t *testing.T, store *Store, n int) {
	t.Helper()
	ctx := context.Background()
	startedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	finished := startedAt.Add(time.Minute)
	if err := store.UpsertRun(ctx, Projection{Run: RunRow{
		RunID: fmt.Sprintf("%032x", n), Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt:  startedAt.Add(time.Duration(n) * time.Minute),
		FinishedAt: &finished, LastSeq: uint64(n + 1),
	}}); err != nil {
		t.Fatalf("seed %d: %v", n, err)
	}
}

// TestFeedResumesFromACursorWithoutInMemoryHistory is the property that lets
// the filesystem poller go.
//
// The old detector kept an in-memory event ring and a random per-process
// session id, so a client could only resume against the process that served it.
// The cursor is durable and process-independent: a brand-new Feed over the same
// store resumes a cursor it has never seen.
func TestFeedResumesFromACursorWithoutInMemoryHistory(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for i := 0; i < 5; i++ {
		seedChange(t, store, i)
	}

	head, err := NewFeed(store).Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 5; i < 8; i++ {
		seedChange(t, store, i)
	}

	// A DIFFERENT feed instance — standing in for a different process.
	page, err := NewFeed(store).Since(ctx, Cursor{
		SchemaVersion: head.SchemaVersion, Epoch: head.Epoch, Seq: head.Seq,
	}, 100)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(page.Changes) != 3 {
		t.Errorf("resumed with %d changes, want 3; resume must not depend on in-memory "+
			"history held by whichever process served the original connection",
			len(page.Changes))
	}
	if page.Cursor.Seq <= head.Seq {
		t.Errorf("cursor did not advance: %d -> %d", head.Seq, page.Cursor.Seq)
	}
}

// TestFeedRefusesEachConditionByName pins that the three conditions are
// independently reachable and named.
//
// One generic "stale cursor" would be useless: the client's correct response
// differs per condition, and a rebuilt store would be indistinguishable from a
// pruned feed or a client that is simply too old.
func TestFeedRefusesEachConditionByName(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for i := 0; i < 6; i++ {
		seedChange(t, store, i)
	}
	feed := NewFeed(store)
	head, err := feed.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name   string
		cursor Cursor
		want   CursorCondition
	}{
		{"epoch", Cursor{SchemaVersion: head.SchemaVersion, Epoch: "someone-elses-epoch", Seq: 1}, CursorEpochChanged},
		{"schema", Cursor{SchemaVersion: head.SchemaVersion + 99, Epoch: head.Epoch, Seq: 1}, CursorSchemaChanged},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := feed.Since(ctx, testCase.cursor, 10)
			var refusal *CursorError
			if !errors.As(err, &refusal) {
				t.Fatalf("got %v, want a typed CursorError", err)
			}
			if refusal.Condition != testCase.want {
				t.Errorf("condition = %q, want %q", refusal.Condition, testCase.want)
			}
			if refusal.Code() != string(testCase.want) {
				t.Errorf("wire code = %q, want %q", refusal.Code(), testCase.want)
			}
		})
	}

	// Truncation needs a floor above the cursor.
	changes, err := store.Changes(ctx, 0, 10)
	if err != nil || len(changes) < 4 {
		t.Fatalf("need changes to prune: %d (%v)", len(changes), err)
	}
	if _, err := store.PruneChanges(ctx, changes[3].Seq); err != nil {
		t.Fatal(err)
	}
	_, err = feed.Since(ctx, Cursor{
		SchemaVersion: head.SchemaVersion, Epoch: head.Epoch, Seq: changes[0].Seq,
	}, 10)
	var refusal *CursorError
	if !errors.As(err, &refusal) || refusal.Condition != CursorFeedTruncated {
		t.Errorf("a cursor below the retention floor gave %v, want %q", err, CursorFeedTruncated)
	}
}

// TestFeedBlocksUntilNotified pins that a caller with nothing to read waits
// rather than spinning.
//
// The old detector polled on a 100ms ticker whether or not anything had
// happened. This wakes on a commit.
func TestFeedBlocksUntilNotified(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := openTestStore(t)
	feed := NewFeed(store)
	head, err := feed.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg   sync.WaitGroup
		page FeedPosition
		read error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		page, read = feed.Since(ctx, head, 10)
	}()

	// Give the reader time to block, then commit and notify.
	time.Sleep(50 * time.Millisecond)
	seedChange(t, store, 42)
	feed.Notify()

	wg.Wait()
	if read != nil {
		t.Fatalf("blocked read: %v", read)
	}
	if len(page.Changes) == 0 {
		t.Error("the waiter was notified but read nothing")
	}
}

// TestFeedRegistersBeforeReading pins the ordering that prevents a lost wakeup.
//
// If the waiter registered AFTER reading, a change committing in that window
// would be missed: the read returns empty, the registration happens, and the
// notify for that change has already fired. The subscriber then blocks until
// some unrelated later commit — on a quiet instance, potentially forever.
func TestFeedRegistersBeforeReading(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := openTestStore(t)
	feed := NewFeed(store)
	head, err := feed.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	readStarted := make(chan struct{})
	allowRead := make(chan struct{})
	feed.readChanges = func(ctx context.Context, seq uint64, limit int) ([]Change, error) {
		close(readStarted)
		select {
		case <-allowRead:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return store.Changes(ctx, seq, limit)
	}

	type result struct {
		page FeedPosition
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := feed.Since(ctx, head, 10)
		done <- result{page: page, err: err}
	}()

	select {
	case <-readStarted:
	case <-ctx.Done():
		t.Fatal("reader did not reach the feed read")
	}
	feed.mu.Lock()
	registered := len(feed.waiters) > 0
	feed.mu.Unlock()
	if !registered {
		t.Fatal("reader started reading before registering a waiter")
	}

	seedChange(t, store, 7)
	feed.Notify()
	close(allowRead)

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("read failed: %v", result.err)
		}
		page := result.page
		if len(page.Changes) == 0 {
			t.Error("read returned no changes")
		}
	case <-time.After(2 * time.Second):
		t.Error("the reader blocked on a change that was already committed; a notify that " +
			"fires before the read must not be lost")
	}
}

func waiterCount(feed *Feed) int {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	return len(feed.waiters)
}

// TestFeedUnregistersWaitersOnEveryExit pins that no path out of Since leaves a
// channel nothing will ever close.
//
// Registration happens before the read, so every early return — a query error,
// rows already available, a cancelled context — used to abandon its channel for
// a Notify that, on a quiet instance, never comes. Repeated SSE
// connect/disconnect then grew memory linearly.
func TestFeedUnregistersWaitersOnEveryExit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	t.Run("query error", func(t *testing.T) {
		feed := NewFeed(store)
		head, err := feed.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		feed.readChanges = func(context.Context, uint64, int) ([]Change, error) {
			return nil, errors.New("read failed")
		}
		for i := 0; i < 10; i++ {
			if _, err := feed.Since(ctx, head, 10); err == nil {
				t.Fatal("want the query error to surface")
			}
		}
		if got := waiterCount(feed); got != 0 {
			t.Errorf("waiters after 10 failed reads = %d, want 0", got)
		}
	})

	t.Run("immediate rows", func(t *testing.T) {
		feed := NewFeed(store)
		head, err := feed.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seedChange(t, store, 100)
		for i := 0; i < 10; i++ {
			if _, err := feed.Since(ctx, head, 10); err != nil {
				t.Fatal(err)
			}
		}
		if got := waiterCount(feed); got != 0 {
			t.Errorf("waiters after 10 immediate reads = %d, want 0", got)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		feed := NewFeed(store)
		head, err := feed.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			cancelled, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			_, err := feed.Since(cancelled, head, 10)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancelled subscription %d returned %v", i, err)
			}
		}
		if got := waiterCount(feed); got != 0 {
			t.Errorf("waiters after 10 cancelled subscriptions = %d, want 0", got)
		}
	})
}

// TestFeedPagedCatchUpDoesNotRetainWaiters pins that a client catching up over
// several pages does not leave one abandoned channel per page.
func TestFeedPagedCatchUpDoesNotRetainWaiters(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	feed := NewFeed(store)
	cursor, err := feed.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		seedChange(t, store, i)
	}

	pages := 0
	for {
		page, err := feed.Since(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		cursor = page.Cursor
		pages++
		changes, err := store.Changes(ctx, cursor.Seq, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) == 0 {
			break
		}
	}
	if pages < 2 {
		t.Fatalf("catch-up took %d pages, want it paged", pages)
	}
	if got := waiterCount(feed); got != 0 {
		t.Errorf("waiters after %d catch-up pages = %d, want 0", pages, got)
	}
}

// TestFeedNotifyRacesWithUnregister pins that unregistering does not cost a
// wakeup: blocked readers must still be woken while other subscriptions are
// being cancelled concurrently.
func TestFeedNotifyRacesWithUnregister(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := openTestStore(t)
	feed := NewFeed(store)
	head, err := feed.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var churn sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		churn.Add(1)
		go func() {
			defer churn.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				short, cancelShort := context.WithTimeout(ctx, time.Millisecond)
				_, _ = feed.Since(short, head, 10)
				cancelShort()
			}
		}()
	}

	var readers sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if _, err := feed.Since(ctx, head, 10); err != nil {
				errs <- err
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	seedChange(t, store, 3)
	for i := 0; i < 20; i++ {
		feed.Notify()
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		readers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked reader was never woken; concurrent unregister must not lose wakeups")
	}
	close(stop)
	churn.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("reader failed: %v", err)
	}
	if got := waiterCount(feed); got != 0 {
		t.Errorf("waiters after the race = %d, want 0", got)
	}
}
