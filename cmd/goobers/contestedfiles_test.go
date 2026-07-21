package main

import (
	"reflect"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestReferencedFilePaths(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "full paths, bare names, and backticked",
			text: "Touches `internal/gate/gate.go` and daemon.go plus merge-review.yaml",
			want: []string{"internal/gate/gate.go", "daemon.go", "merge-review.yaml"},
		},
		{
			name: "strips leading ./ and dedupes",
			text: "see ./cmd/goobers/run.go and again cmd/goobers/run.go",
			want: []string{"cmd/goobers/run.go"},
		},
		{
			name: "ignores version strings and prose",
			text: "bump to v1.2.3, e.g. nothing here, schema v0.1",
			want: []string{},
		},
		{
			name: "recognizes yml/json/ts",
			text: "config.yml, api/schema.json, portal/app.tsx",
			want: []string{"config.yml", "api/schema.json", "portal/app.tsx"},
		},
		{
			name: "no false match on longer extension",
			text: "the word gopher and golang are not files",
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := referencedFilePaths(tc.text)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("referencedFilePaths(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestFileRefMatchesPath(t *testing.T) {
	cases := []struct {
		prPath string
		ref    string
		want   bool
	}{
		{"internal/runner/run.go", "internal/runner/run.go", true}, // exact
		{"internal/runner/run.go", "run.go", true},                 // bare basename suffix
		{"internal/runner/run.go", "runner/run.go", true},          // partial path suffix
		{"internal/runner/delegate.go", "gate.go", false},          // suffix must be /-aligned
		{"cmd/goobers/run.go", "internal/runner/run.go", false},    // different full path
		{"daemon.go", "daemon.go", true},                           // top-level exact
	}
	for _, tc := range cases {
		if got := fileRefMatchesPath(tc.prPath, tc.ref); got != tc.want {
			t.Errorf("fileRefMatchesPath(%q, %q) = %v, want %v", tc.prPath, tc.ref, got, tc.want)
		}
	}
}

func TestDistinctPRsTouchingRefs(t *testing.T) {
	touches := []openPRTouch{
		{number: 1, files: []string{"internal/gate/gate.go", "a.go"}},
		{number: 2, files: []string{"internal/gate/gate.go"}},
		{number: 3, files: []string{"cmd/goobers/backlogquery.go"}},
	}
	if got := distinctPRsTouchingRefs([]string{"gate.go"}, touches); got != 2 {
		t.Errorf("gate.go touched by = %d, want 2", got)
	}
	if got := distinctPRsTouchingRefs([]string{"backlogquery.go"}, touches); got != 1 {
		t.Errorf("backlogquery.go touched by = %d, want 1", got)
	}
	if got := distinctPRsTouchingRefs(nil, touches); got != 0 {
		t.Errorf("no refs touched by = %d, want 0", got)
	}
	if got := distinctPRsTouchingRefs([]string{"nowhere.go"}, touches); got != 0 {
		t.Errorf("nowhere.go touched by = %d, want 0", got)
	}
}

func item(id, title, body string) providers.WorkItem {
	return providers.WorkItem{ID: id, Title: title, Body: body}
}

func TestPartitionByContention(t *testing.T) {
	touches := []openPRTouch{
		{number: 10, files: []string{"internal/gate/gate.go"}},
		{number: 11, files: []string{"internal/gate/gate.go"}},
	}
	eligible := []providers.WorkItem{
		item("100", "clean older", "touches internal/telemetry/query.go"),
		item("101", "contested", "reworks internal/gate/gate.go"),
		item("102", "clean newer", "no file references at all"),
	}

	ordered, contested := partitionByContention(eligible, touches, 2)

	wantIDs := []string{"100", "102", "101"} // clean (FIFO) then contested
	var gotIDs []string
	for _, it := range ordered {
		gotIDs = append(gotIDs, it.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("order = %v, want %v", gotIDs, wantIDs)
	}
	if !reflect.DeepEqual(contested, []string{"101"}) {
		t.Fatalf("contested = %v, want [101]", contested)
	}
}

func TestPartitionByContentionAllContestedIsFIFOStable(t *testing.T) {
	// When every candidate is contested, order is unchanged (no starvation:
	// FIFO claiming still proceeds).
	touches := []openPRTouch{
		{number: 1, files: []string{"a.go"}},
		{number: 2, files: []string{"a.go"}},
	}
	eligible := []providers.WorkItem{
		item("1", "first", "a.go"),
		item("2", "second", "a.go"),
	}
	ordered, contested := partitionByContention(eligible, touches, 2)
	if ordered[0].ID != "1" || ordered[1].ID != "2" {
		t.Fatalf("order = %v, want stable [1 2]", []string{ordered[0].ID, ordered[1].ID})
	}
	if len(contested) != 2 {
		t.Fatalf("contested count = %d, want 2", len(contested))
	}
}

func TestPartitionByContentionBelowThresholdIsClean(t *testing.T) {
	// A file touched by only one PR is below the default 2-PR threshold, so
	// the candidate is not deprioritized.
	touches := []openPRTouch{{number: 1, files: []string{"a.go"}}}
	eligible := []providers.WorkItem{
		item("1", "one", "a.go"),
		item("2", "two", "b.go"),
	}
	ordered, contested := partitionByContention(eligible, touches, 2)
	if len(contested) != 0 {
		t.Fatalf("contested = %v, want none (below threshold)", contested)
	}
	if ordered[0].ID != "1" || ordered[1].ID != "2" {
		t.Fatalf("order = %v, want unchanged", []string{ordered[0].ID, ordered[1].ID})
	}
}
