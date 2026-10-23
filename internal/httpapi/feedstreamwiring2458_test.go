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

	backend := struct{ readmodel.Backend }{Backend: store}
	config := &handlerConfig{}
	if err := WithChangeFeedStream(backend, store.Feed())(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.events.Close)
	stream, ok := config.events.(*feedStream)
	if !ok || stream.feed != store.Feed() {
		t.Fatal("handler did not retain the store's singleton feed")
	}

	foreign := &handlerConfig{}
	if err := WithChangeFeedStream(backend, readmodel.NewFeed(store))(foreign); err == nil {
		if foreign.events != nil {
			foreign.events.Close()
		}
		t.Fatal("handler accepted a second feed: projector wakeups would be lost")
	}
	if foreign.events != nil {
		t.Fatal("refused feed was installed")
	}

}

func TestWithChangeFeedStreamRequiresBothCapabilities(t *testing.T) {
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for name, option := range map[string]HandlerOption{
		"backend": WithChangeFeedStream(nil, store.Feed()),
		"feed":    WithChangeFeedStream(store, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := option(&handlerConfig{}); err == nil {
				t.Fatal("incomplete change-feed capabilities accepted")
			}
		})
	}
}
