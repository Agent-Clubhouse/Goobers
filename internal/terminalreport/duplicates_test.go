package terminalreport

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type unsupportedDuplicatePoster struct{ frozenBodyPoster }

func (*unsupportedDuplicatePoster) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return gate.ErrCommentUpdateUnsupported
}

func TestADODuplicateReportsAreJournaledWithoutMutation(t *testing.T) {
	e := Effect{Kind: WorkFailure, Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"}, ItemID: "42", RunID: "run-2", Count: 2}
	p := &unsupportedDuplicatePoster{}
	apply := func(ctx context.Context, e Effect) error { return e.Apply(ctx, p) }
	if err := apply(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	p.comments[0].ID = "10"
	duplicate := p.comments[0]
	duplicate.ID = "20"
	foreign := duplicate
	foreign.ID = "1"
	foreign.AuthorID = "other-guid"
	p.comments = append(p.comments, duplicate, foreign)
	before := append([]providers.Comment(nil), p.comments...)
	var events []journal.Event
	q := Queue{Dir: t.TempDir(), Log: func(e journal.Event) error { events = append(events, e); return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Publish(ctx, e, apply); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	record := q.Log
	q.Log = func(journal.Event) error { return errors.New("journal unavailable") }
	if err := q.Reconcile(context.Background(), 1, apply); err == nil {
		t.Fatal("lost unsupported-cleanup diagnostic on journal failure")
	}
	q.Log = record
	if err := q.Reconcile(context.Background(), 1, apply); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, p.comments) || len(p.bodies) != 1 {
		t.Fatal("unsupported ADO cleanup mutated or duplicated a comment")
	}
	if len(events) != 1 || events[0].Runner["kind"] != "terminal_report_duplicate_cleanup_unsupported" || events[0].Runner["canonicalCommentId"] != "10" || !reflect.DeepEqual(events[0].Runner["duplicateCommentIds"], []string{"20"}) {
		t.Fatalf("missing actionable diagnostic: %+v", events)
	}
	if err := q.Reconcile(context.Background(), 1, func(context.Context, Effect) error {
		t.Fatal("already journaled unsupported cleanup retried")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
