package blockedrecordspg

import (
	"context"
	"errors"
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

func node(provider providers.ProviderKind, id string) blockedcycle.Node {
	return blockedcycle.Node{Repository: providers.RepositoryRef{Provider: provider, Owner: "acme", Name: "web"}, ItemID: id}
}

func TestBackoffRetainDropsNodesNoRecordMentions(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &Backoff{Now: func() time.Time { return now }}
	keep, gone := node(providers.ProviderGitHub, "1"), node(providers.ProviderGitHub, "2")
	b.Failed(keep)
	b.Failed(gone)
	b.Retain(map[blockedcycle.Node]bool{keep: true})
	if b.Allowed(keep) {
		t.Fatal("a live node lost its backoff")
	}
	if !b.Allowed(gone) {
		t.Fatal("a node no record mentions kept its backoff entry")
	}
}

func TestReadyNodesDropsOnlyTheNodesInBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &Backoff{Now: func() time.Time { return now }}
	nodes := []blockedcycle.Node{node(providers.ProviderGitHub, "1"), node(providers.ProviderGitHub, "2"), node(providers.ProviderGitHub, "3")}
	b.Failed(nodes[1])
	ready := readyNodes(nodes, b)
	if len(ready) != 2 || ready[0] != nodes[0] || ready[1] != nodes[2] {
		t.Fatalf("ready = %v, want 1 and 3 in order", ready)
	}
	if len(readyNodes(nodes, nil)) != 3 {
		t.Fatal("a nil backoff dropped nodes")
	}
}

type probe struct {
	calls   []string
	closed  map[string]bool
	failing map[string]bool
}

func (p *probe) closedFunc(_ context.Context, _ string, _ providers.RepositoryRef, id string) (bool, error) {
	p.calls = append(p.calls, id)
	if p.failing[id] {
		return false, errors.New("provider unavailable")
	}
	return p.closed[id], nil
}

func TestClosedNodesAccountsEveryReadAndSkipsOnlyASpentProvider(t *testing.T) {
	gh1, gh2, ado := node(providers.ProviderGitHub, "1"), node(providers.ProviderGitHub, "2"), node(providers.ProviderADO, "3")
	p := &probe{closed: map[string]bool{"1": true, "2": true, "3": true}}
	var reserved []providers.ProviderKind
	store := Store{Closed: p.closedFunc, Reserve: func(k providers.ProviderKind) bool {
		reserved = append(reserved, k)
		return k != providers.ProviderGitHub // GitHub's budget is spent
	}}
	closed, skipped, _ := store.closedNodes(t.Context(), nil, []blockedcycle.Node{gh1, gh2, ado}, "run", false)
	if len(skipped) != 2 || skipped[0] != gh1 || skipped[1] != gh2 {
		t.Fatalf("skipped = %v, want the spent provider's two items reported unread", skipped)
	}
	if len(closed) != 1 || closed[0] != ado {
		t.Fatalf("closed = %v; want only the other provider's item checked (a spent provider must not stop the rest)", closed)
	}
	if len(p.calls) != 1 || p.calls[0] != "3" {
		t.Fatalf("provider reads = %v; want none for the spent provider", p.calls)
	}
	if len(reserved) != 2 {
		t.Fatalf("reservations = %v; want one per provider with the spent one asked once", reserved)
	}
	// With no budget at all, nothing is read.
	p.calls = nil
	none := Store{Closed: p.closedFunc, Reserve: func(providers.ProviderKind) bool { return false }}
	if closed, skipped, _ := none.closedNodes(t.Context(), nil, []blockedcycle.Node{gh1, ado}, "run", false); len(closed) != 0 || len(skipped) != 2 || len(p.calls) != 0 {
		t.Fatalf("with no budget: closed %v skipped %v reads %v", closed, skipped, p.calls)
	}
}

func TestClosedNodesHonoursTheBackoffOnlyWhenAsked(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &Backoff{Now: func() time.Time { return now }}
	n := node(providers.ProviderGitHub, "7")
	b.Failed(n)
	p := &probe{closed: map[string]bool{"7": true}}
	store := Store{Closed: p.closedFunc, Backoff: b}
	if closed, _, _ := store.closedNodes(t.Context(), nil, []blockedcycle.Node{n}, "run", true); len(closed) != 0 || len(p.calls) != 0 {
		t.Fatalf("the heal read a backed-off item: closed %v reads %v", closed, p.calls)
	}
	if closed, _, _ := store.closedNodes(t.Context(), nil, []blockedcycle.Node{n}, "run", false); len(closed) != 1 || len(p.calls) != 1 {
		t.Fatalf("a block being decided skipped a backed-off item: closed %v reads %v", closed, p.calls)
	}
	if !b.Allowed(n) {
		t.Fatal("a verified read did not clear the item's backoff")
	}
	// A failing read backs the item off.
	p.failing = map[string]bool{"7": true}
	_, _, _ = store.closedNodes(t.Context(), nil, []blockedcycle.Node{n}, "run", false)
	if b.Allowed(n) {
		t.Fatal("a failed read did not back the item off")
	}
}
