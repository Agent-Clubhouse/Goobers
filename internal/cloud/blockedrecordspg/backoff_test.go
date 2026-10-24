package blockedrecordspg

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/providers"
)

func TestBackoffDelaysAFailedItemAndDoublesUpToTheCap(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &Backoff{Base: time.Minute, Max: 8 * time.Minute, Now: func() time.Time { return now }}
	node := blockedcycle.Node{Repository: providers.RepositoryRef{Name: "web"}, ItemID: "7"}
	other := blockedcycle.Node{Repository: providers.RepositoryRef{Name: "web"}, ItemID: "8"}
	if !b.Allowed(node) {
		t.Fatal("a fresh item was held back")
	}
	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 8 * time.Minute} {
		b.Failed(node)
		if b.Allowed(node) {
			t.Fatalf("failure %d: allowed immediately", i+1)
		}
		now = now.Add(wait - time.Nanosecond)
		if b.Allowed(node) {
			t.Fatalf("failure %d: allowed %v after, before the %v backoff", i+1, wait-time.Nanosecond, wait)
		}
		now = now.Add(time.Nanosecond)
		if !b.Allowed(node) {
			t.Fatalf("failure %d: still held after the %v backoff", i+1, wait)
		}
	}
	if !b.Allowed(other) {
		t.Fatal("one item's failure held back another")
	}
	b.Failed(node)
	b.Verified(node)
	if !b.Allowed(node) {
		t.Fatal("a verified item stayed in backoff")
	}
	var none *Backoff
	none.Failed(node)
	none.Verified(node)
	if !none.Allowed(node) {
		t.Fatal("a nil Backoff held an item back")
	}
}
