package remediation

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/api/integrity"
)

func TestRetrieveTopKPreservesCompleteResults(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	query := Query{Stage: "review", ErrorClass: "timeout", FailureExcerpt: "review timeout", ConfigDigest: "current"}
	newRecord := func(id, fix string) Record {
		return Record{
			ID: id, Stage: query.Stage, ErrorClass: query.ErrorClass, FailureExcerpt: query.FailureExcerpt,
			FixExcerpt: fix, DidItHelp: true, OutcomeKnown: true, ObservedAt: now,
			ConfigDigest: query.ConfigDigest, Integrity: integrity.Trusted,
		}
	}
	bestZ := newRecord("z-best", "retry the bounded poll")
	bestA := newRecord("a-best", "restart the reviewer")
	bestASecond := newRecord("a-best", "retry with the same identity")
	oldConfig := newRecord("old-config", "restore the previous configuration")
	oldConfig.ConfigDigest = "old"
	recent := newRecord("recent", "use a longer deadline")
	recent.ObservedAt = now.Add(-90 * 24 * time.Hour)
	unsuccessful := newRecord("unsuccessful", "skip the review")
	unsuccessful.DidItHelp = false
	stale := newRecord("stale", "increase polling timeout")
	stale.ConfigDigest = "old"
	stale.ObservedAt = now.Add(-180 * 24 * time.Hour)
	records := []Record{stale, bestZ, recent, bestA, unsuccessful, oldConfig, bestASecond}
	index := NewIndex(records, nil)
	corpusBefore := append([]indexedRecord(nil), index.records...)
	wantAll := []Result{
		{Record: bestA, Score: 1, FewShot: "Past failure (review/timeout), did-it-help: true\nFailure: review timeout\nVerified fix: restart the reviewer"},
		{Record: bestASecond, Score: 1, FewShot: "Past failure (review/timeout), did-it-help: true\nFailure: review timeout\nVerified fix: retry with the same identity"},
		{Record: bestZ, Score: 1, FewShot: "Past failure (review/timeout), did-it-help: true\nFailure: review timeout\nVerified fix: retry the bounded poll"},
		{Record: oldConfig, Score: 0.5, FewShot: "Past failure (review/timeout), did-it-help: true\nFailure: review timeout\nVerified fix: restore the previous configuration"},
		{Record: recent, Score: 0.5, FewShot: "Past failure (review/timeout), did-it-help: true\nFailure: review timeout\nVerified fix: use a longer deadline"},
		{Record: unsuccessful, Score: 0.25, FewShot: "Past failure (review/timeout), did-it-help: false\nFailure: review timeout\nVerified fix: skip the review"},
		{Record: stale, Score: 0.125, FewShot: "Past failure (review/timeout), did-it-help: true\nFailure: review timeout\nVerified fix: increase polling timeout"},
	}
	for _, k := range []int{-1, 0, 1, 2, 3, 5, len(records), len(records) + 1} {
		t.Run(fmt.Sprintf("K=%d", k), func(t *testing.T) {
			limit := k
			if limit <= 0 {
				limit = DefaultK
			}
			limit = min(limit, len(wantAll))
			got := index.Retrieve(query, Options{K: k, Now: now})
			if !reflect.DeepEqual(got, wantAll[:limit]) {
				t.Fatalf("results = %+v, want %+v", got, wantAll[:limit])
			}
			if !reflect.DeepEqual(index.records, corpusBefore) {
				t.Fatal("retrieval mutated the index")
			}
		})
	}
}

func TestRetrieveTopKWithoutCandidates(t *testing.T) {
	var nilIndex *Index
	if got := nilIndex.Retrieve(Query{}, Options{K: 1}); got != nil {
		t.Fatalf("nil index results = %+v, want nil", got)
	}
	if got := NewIndex(nil, nil).Retrieve(Query{}, Options{K: 1}); got != nil {
		t.Fatalf("empty index results = %+v, want nil", got)
	}
	index := NewIndex([]Record{{
		ID: "record", Stage: "review", FailureExcerpt: "timeout", FixExcerpt: "retry",
		DidItHelp: true, OutcomeKnown: true, Integrity: integrity.Trusted,
	}}, nil)
	if got := index.Retrieve(Query{}, Options{K: 1}); got == nil || len(got) != 0 {
		t.Fatalf("zero-similarity results = %+v, want non-nil empty slice", got)
	}
}
