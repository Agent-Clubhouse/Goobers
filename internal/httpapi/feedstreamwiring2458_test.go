package httpapi

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/readmodel"
)

// TestFeedStreamUsesTheStoreFeed pins the call site #2458 was actually broken
// at, which the readmodel-side singleton test cannot reach.
//
// Production built one feed for the projector and a second, here, for the SSE
// stream. Both call sites looked right; the defect lived between them. A
// regression would look exactly like the original code — a plain
// readmodel.NewFeed(store) in newFeedStream — so this asserts the identity
// rather than any behaviour that identity happens to produce.
func TestFeedStreamUsesTheStoreFeed(t *testing.T) {
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stream := newFeedStream(store)
	t.Cleanup(stream.Close)

	if stream.feed != store.Feed() {
		t.Fatal("the SSE stream holds a different feed from the store's: a projector commit notifies " +
			"the store's feed while Since waits here, so the wakeup is lost and the Portal stays stale")
	}
}
