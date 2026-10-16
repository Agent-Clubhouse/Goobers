package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChildCancellationTimeRequiresExactRetainedLineage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	c := acceptChildTest(t, s, childRequest("parent", "stage", "key"), childTestTime)
	if _, err := s.ChildCancellationTime(t.Context(), c.Identity); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("invented cancellation", err)
	}
	at := childTestTime.Add(time.Minute)
	if err := s.FenceChildParent(t.Context(), c.Identity.ChildParent, "human", at); err != nil {
		t.Fatal(err)
	}
	if err := s.FenceChildParent(t.Context(), c.Identity.ChildParent, "retry", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	got, err := s.ChildCancellationTime(t.Context(), c.Identity)
	if err != nil || !got.Equal(at) {
		t.Fatal("first cancellation clock changed", got, err)
	}
	for _, change := range []func(*ChildIdentity){
		func(i *ChildIdentity) { i.Gaggle = "foreign" },
		func(i *ChildIdentity) { i.ParentRunID = "foreign" },
		func(i *ChildIdentity) { i.StageOccurrence = "foreign" },
		func(i *ChildIdentity) { i.InvocationKey = "foreign" },
	} {
		other := c.Identity
		change(&other)
		if _, err := s.ChildCancellationTime(t.Context(), other); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("foreign lineage inherited cancellation", err)
		}
	}
}
