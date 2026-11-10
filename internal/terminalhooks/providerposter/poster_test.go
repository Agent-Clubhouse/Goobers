package providerposter

import (
	"context"
	"errors"
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

type adoWriter struct {
	providerwriter.Pending
	reqs []providers.UpdateWorkItemRequest
	fail func(providers.UpdateWorkItemRequest) error
}

func (w *adoWriter) Update(_ context.Context, _ providerwriter.ItemRef, req providers.UpdateWorkItemRequest) error {
	w.reqs = append(w.reqs, req)
	if w.fail != nil {
		return w.fail(req)
	}
	return nil
}

// Azure Boards refuses a comment, and with it every label in the same request.
// The labels park or escalate the item, so on ADO they go first on their own.
func TestAnADOCommentRequestSendsItsLabelsFirstAndSeparately(t *testing.T) {
	refuseComments := func(r providers.UpdateWorkItemRequest) error {
		if r.Comment != "" {
			return providerwriter.ErrPermanent
		}
		return nil
	}
	w := &adoWriter{fail: refuseComments}
	p := Poster{Writer: w, RunID: "run-1"}
	_, err := p.UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "board"}, ID: "7", Comment: "report",
		AddLabels: []string{providers.LabelNeedsHuman}, RemoveLabels: []string{providers.LabelReady},
	})
	if !providerwriter.Permanent(err) {
		t.Fatalf("err = %v, want the comment's refusal reported", err)
	}
	if len(w.reqs) != 2 || len(w.reqs[0].AddLabels) != 1 || w.reqs[0].Comment != "" || w.reqs[0].IdempotencyKey != "" {
		t.Fatalf("requests = %+v; want a labels-only request first", w.reqs)
	}
	if len(w.reqs[1].AddLabels) != 0 || len(w.reqs[1].RemoveLabels) != 0 || w.reqs[1].Comment != "report" || w.reqs[1].IdempotencyKey == "" {
		t.Fatalf("second request = %+v; want the keyed comment alone", w.reqs[1])
	}
	// A refused label stops before the comment is attempted.
	w = &adoWriter{fail: func(providers.UpdateWorkItemRequest) error { return errors.New("down") }}
	if _, err := (Poster{Writer: w, RunID: "r"}).UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO}, ID: "7", Comment: "c", AddLabels: []string{"x"},
	}); err == nil || len(w.reqs) != 1 {
		t.Fatalf("err %v after %d requests; want the label failure to stop it", err, len(w.reqs))
	}
	// GitHub keeps its single request.
	w = &adoWriter{}
	if _, err := (Poster{Writer: w, RunID: "r"}).UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub}, ID: "7", Comment: "c", AddLabels: []string{"x"},
	}); err != nil || len(w.reqs) != 1 || w.reqs[0].Comment != "c" || len(w.reqs[0].AddLabels) != 1 {
		t.Fatalf("GitHub requests = %+v, %v; want one combined request", w.reqs, err)
	}
	// An ADO comment without labels is left alone.
	w = &adoWriter{}
	if _, err := (Poster{Writer: w, RunID: "r"}).UpdateWorkItem(t.Context(), providers.UpdateWorkItemRequest{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO}, ID: "7", Comment: "c",
	}); err != nil || len(w.reqs) != 1 {
		t.Fatalf("ADO comment-only requests = %+v, %v", w.reqs, err)
	}
}
