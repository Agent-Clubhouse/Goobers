package readmodel

import (
	"context"
	"testing"
	"time"
)

// TestStoreFeedIsASingleton is #2458's structural pin.
//
// Production built one feed for the projector and a second for the SSE stream.
// A Feed delivers its wakeup through the waiter set held on the instance
// Notify is called on, so the two gave writers and subscribers different
// rendezvous points: Since waited on the stream's channel while commits
// notified only the projector's, and the Portal stayed stale until something
// unrelated woke it or the client reconnected.
func TestStoreFeedIsASingleton(t *testing.T) {
	store := openTestStore(t)

	first := store.Feed()
	second := store.Feed()
	if first != second {
		t.Fatal("Store.Feed returned two instances; a writer and a subscriber would take different " +
			"rendezvous points and the wakeup would be lost")
	}
}

// TestNotifyOnTheStoreFeedWakesAStoreFeedWaiter is the behaviour that pin
// protects, asserted through the wakeup itself rather than through pointer
// identity: a subscriber waiting on the store's feed must be woken by a
// Notify on the store's feed.
func TestNotifyOnTheStoreFeedWakesAStoreFeedWaiter(t *testing.T) {
	store := openTestStore(t)

	waiter, unregister := store.Feed().wait()
	defer unregister()

	// A second caller asking the store for "its" feed and notifying is exactly
	// what the projector does after a commit.
	store.Feed().Notify()

	select {
	case <-waiter:
	case <-time.After(2 * time.Second):
		t.Fatal("a subscriber on the store's feed was not woken by a Notify on the store's feed")
	}
}

// TestSeparateFeedsOverOneStoreDoNotShareWakeups documents WHY the singleton
// is required rather than merely tidy, by exercising the shape the old wiring
// had. If this ever stops holding — if Feed grows a store-level registry of
// waiters — the singleton is no longer load-bearing and this test should be
// the thing that says so.
func TestSeparateFeedsOverOneStoreDoNotShareWakeups(t *testing.T) {
	store := openTestStore(t)

	projectorFeed := NewFeed(store)
	streamFeed := NewFeed(store)

	waiter, unregister := streamFeed.wait()
	defer unregister()

	projectorFeed.Notify()

	select {
	case <-waiter:
		t.Fatal("a Notify on one feed woke a waiter on another; the #2458 singleton would no longer " +
			"be load-bearing, so re-derive whether the wiring still needs it")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestFeedHeadIsReadableFromTheStoreFeed keeps the singleton honest about
// being a working feed rather than merely a shared pointer.
func TestFeedHeadIsReadableFromTheStoreFeed(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Feed().Head(context.Background()); err != nil {
		t.Fatalf("Head on the store's feed: %v", err)
	}
}
