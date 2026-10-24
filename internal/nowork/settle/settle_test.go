package settle

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/providers"
)

type poster struct{ reqs []providers.UpdateWorkItemRequest }

func (p *poster) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.reqs = append(p.reqs, req)
	return providers.WorkItem{}, nil
}

type fixture struct {
	items    []Item
	terminal nowork.Terminal
	noWork   bool
	termErr  error
	records  map[string]nowork.Record
}

func (f *fixture) deps() Deps {
	return Deps{
		Items:    func(string) ([]Item, error) { return f.items, nil },
		Terminal: func(string, string) (nowork.Terminal, bool, error) { return f.terminal, f.noWork, f.termErr },
		Increment: func(_ context.Context, _ providers.RepositoryRef, id, run string, t nowork.Terminal) (nowork.Record, nowork.Record, error) {
			prev := f.records[id]
			next := nowork.Advance(prev, t, run, time.Unix(1, 0))
			f.records[id] = next
			return next, prev, nil
		},
		Reset: func(_ context.Context, _ providers.RepositoryRef, id, _ string) (nowork.Record, error) {
			cleared := f.records[id]
			delete(f.records, id)
			return cleared, nil
		},
	}
}

func newFixture() *fixture {
	return &fixture{
		items:    []Item{{ItemID: "7", Repo: providers.RepositoryRef{Name: "web"}}},
		terminal: nowork.Terminal{Stage: "implement", Reason: "nothing to do"},
		noWork:   true,
		records:  map[string]nowork.Record{},
	}
}

func TestRepeatedNoWorkParksAtTheThresholdAndProductiveCompletionResets(t *testing.T) {
	f, p := newFixture(), &poster{}
	for i := 0; i < nowork.StreakThreshold; i++ {
		if err := Settle(t.Context(), p, f.deps(), "run", "implement", ""); err != nil {
			t.Fatal(err)
		}
	}
	last := p.reqs[len(p.reqs)-1]
	if !slices.Contains(last.AddLabels, providers.LabelNeedsHuman) || !slices.Contains(last.RemoveLabels, providers.LabelReady) {
		t.Fatalf("threshold request = %+v, want the park", last)
	}
	for _, r := range p.reqs[:len(p.reqs)-1] {
		if len(r.AddLabels) != 0 {
			t.Fatalf("parked below the threshold: %+v", r)
		}
	}
	f.noWork = false
	if err := Settle(t.Context(), p, f.deps(), "run", "implement", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 0 {
		t.Fatalf("productive completion did not reset: %+v", f.records)
	}
}

func TestBatchAndCurationClaimsAreNeverCounted(t *testing.T) {
	for name, items := range map[string][]Item{
		"batch":    {{ItemID: "1"}, {ItemID: "2"}},
		"curation": {{ItemID: "1", Curation: true}},
	} {
		f, p := newFixture(), &poster{}
		f.items = items
		if err := Settle(t.Context(), p, f.deps(), "run", "implement", ""); err != nil || len(f.records) != 0 || len(p.reqs) != 0 {
			t.Errorf("%s: err %v records %v requests %v", name, err, f.records, p.reqs)
		}
	}
}

func TestAnUnreadableTerminalLeavesTheStreakAlone(t *testing.T) {
	f, p := newFixture(), &poster{}
	f.records["7"] = nowork.Record{Count: 2}
	f.termErr = errors.New("journal pruned")
	if err := Settle(t.Context(), p, f.deps(), "run", "implement", ""); err != nil {
		t.Fatalf("an unreadable journal was reported: %v", err)
	}
	if f.records["7"].Count != 2 || len(p.reqs) != 0 {
		t.Fatalf("streak or item changed: %+v %v", f.records, p.reqs)
	}
}
