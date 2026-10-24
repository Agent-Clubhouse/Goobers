package providerposter

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

var (
	_ gate.Commenter      = Poster{}
	_ gate.WorkItemReader = Poster{}
	_ parking.Poster      = Poster{}
)

type writer struct {
	providerwriter.Pending
	refs []providerwriter.ItemRef
	reqs []providers.UpdateWorkItemRequest
}

func (w *writer) Update(_ context.Context, ref providerwriter.ItemRef, req providers.UpdateWorkItemRequest) error {
	w.refs, w.reqs = append(w.refs, ref), append(w.reqs, req)
	return nil
}

func TestCommentsAreKeyedPerRunItemAndTextAndCallerKeysWin(t *testing.T) {
	w := &writer{}
	p := Poster{Writer: w, RunID: "run-1"}
	repo := providers.RepositoryRef{Name: "web"}
	for _, req := range []providers.UpdateWorkItemRequest{
		{Repository: repo, ID: "1", Comment: "a"},
		{Repository: repo, ID: "1", Comment: "b"},
		{Repository: repo, ID: "2", Comment: "a"},
		{Repository: repo, ID: "1", Comment: "a", IdempotencyKey: "mine"},
		{Repository: repo, ID: "1", AddLabels: []string{"x"}},
	} {
		if _, err := p.UpdateWorkItem(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]bool{}
	for _, r := range w.reqs[:3] {
		if !strings.HasPrefix(r.IdempotencyKey, "terminal/run-1/") {
			t.Fatalf("unkeyed comment: %+v", r)
		}
		keys[r.IdempotencyKey] = true
	}
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want one per (item, text)", keys)
	}
	if again := IdempotencyKey("run-1", "1", "a"); again != w.reqs[0].IdempotencyKey {
		t.Fatal("the key is not stable across retries")
	}
	if w.reqs[3].IdempotencyKey != "mine" || w.reqs[4].IdempotencyKey != "" {
		t.Fatalf("caller key or label-only request rewritten: %+v %+v", w.reqs[3], w.reqs[4])
	}
	if w.refs[0].RunID != "run-1" || w.refs[0].ItemID != "1" || w.refs[0].Repo != repo {
		t.Fatalf("ref = %+v", w.refs[0])
	}
}

func TestPendingWriterSurfacesErrPending(t *testing.T) {
	p := Poster{Writer: providerwriter.Pending{}, RunID: "r"}
	if _, err := p.UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{ID: "1"}); err == nil {
		t.Fatal("pending writer accepted a write")
	}
}
