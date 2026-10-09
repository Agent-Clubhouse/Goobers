package backlogscope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/providers"
)

// pagedLister serves fixed provider-shaped pages keyed by request cursor and
// records every request it receives.
type pagedLister struct {
	pages    map[string]listerPage
	requests []providers.ListWorkItemsRequest
}

type listerPage struct {
	items      []providers.WorkItem
	candidates int
	next       string
	err        error
	// waitForDone blocks the page until the caller's context ends.
	waitForDone bool
}

func (l *pagedLister) ListWorkItems(ctx context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	l.requests = append(l.requests, req)
	page, ok := l.pages[req.Cursor]
	if !ok {
		return nil, fmt.Errorf("unexpected cursor %q", req.Cursor)
	}
	if page.waitForDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if page.err != nil {
		return nil, page.err
	}
	req.PageInfo.CandidateCount = page.candidates
	req.PageInfo.HasNext = page.next != ""
	req.PageInfo.NextCursor = page.next
	return page.items, nil
}

func item(id, state string, labels ...string) providers.WorkItem {
	return providers.WorkItem{
		Provider: providers.ProviderGitHub, ID: id, State: state, Labels: labels,
		Fields: fieldpredicate.Fields{"type": "Task"},
	}
}

func itemIDs(items []providers.WorkItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func TestCollectSendsScopeAndPagesToCompletion(t *testing.T) {
	predicate, err := fieldpredicate.Compile(`fields["type"] == "Task"`)
	if err != nil {
		t.Fatal(err)
	}
	foreign := item("4", "open", "approved")
	foreign.Provider = providers.ProviderADO
	untyped := item("5", "open", "approved")
	untyped.Fields = fieldpredicate.Fields{"type": "Bug"}
	lister := &pagedLister{pages: map[string]listerPage{
		"": {items: []providers.WorkItem{item("1", "open", "Approved"), item("2", "closed", "approved")}, candidates: 2, next: "2"},
		"2": {items: []providers.WorkItem{
			item("1", "open", "approved"), // repeated across pages
			item("3", "open", "approved-later"),
			foreign,
			untyped,
			item("6", "open", "approved"),
		}, candidates: 5},
	}}
	scope := Scope{
		Repository:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "r"},
		Labels:         []string{"approved"},
		FieldPredicate: predicate,
		State:          StateOpen,
	}
	items, coverage, err := Collect(context.Background(), lister, scope, 1000)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := itemIDs(items); !slices.Equal(got, []string{"1", "6"}) {
		t.Fatalf("items = %v, want [1 6]", got)
	}
	want := Coverage{Status: StatusComplete, ExaminedCandidates: 7, MaxCandidates: 1000, Pages: 2, ScopeMismatches: 5, Consistency: ConsistencyPaged}
	if coverage != want {
		t.Fatalf("coverage = %+v, want %+v", coverage, want)
	}
	if len(lister.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(lister.requests))
	}
	for _, req := range lister.requests {
		if req.State != StateOpen || !slices.Equal(req.Labels, []string{"approved"}) || req.FieldPredicate != predicate ||
			req.Repository != scope.Repository || req.PageInfo == nil || !req.OldestFirst || req.Limit != PageSize {
			t.Fatalf("request = %+v, want the full scope with paging", req)
		}
	}
	if lister.requests[1].Cursor != "2" {
		t.Fatalf("second cursor = %q, want 2", lister.requests[1].Cursor)
	}
}

func TestCollectAllStateKeepsClosedHistory(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{
		"": {items: []providers.WorkItem{item("1", "open"), item("2", "closed")}, candidates: 2},
	}}
	items, coverage, err := Collect(context.Background(), lister, Scope{State: StateAll}, 10)
	if err != nil || !coverage.Complete() {
		t.Fatalf("Collect: coverage=%+v err=%v", coverage, err)
	}
	if got := itemIDs(items); !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("items = %v, want open and closed", got)
	}
	if lister.requests[0].State != StateAll {
		t.Fatalf("state = %q, want all", lister.requests[0].State)
	}
}

func TestCollectEmptyResultIsComplete(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{"": {}}}
	items, coverage, err := Collect(context.Background(), lister, Scope{State: StateOpen}, 10)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(items) != 0 || !coverage.Complete() || coverage.Pages != 1 {
		t.Fatalf("items=%v coverage=%+v, want empty complete", items, coverage)
	}
}

func TestCollectFailsOnUnavailablePage(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{
		"":  {items: []providers.WorkItem{item("1", "open")}, candidates: 1, next: "1"},
		"1": {err: errors.New("503 service unavailable")},
	}}
	items, _, err := Collect(context.Background(), lister, Scope{State: StateOpen}, 10)
	if err == nil || items != nil {
		t.Fatalf("Collect = %v, %v; want an error and no partial items", items, err)
	}
}

func TestCollectRejectsNonAdvancingCursor(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{
		"":  {items: []providers.WorkItem{item("1", "open")}, candidates: 1, next: "1"},
		"1": {items: []providers.WorkItem{item("2", "open")}, candidates: 1, next: "1"},
	}}
	if _, _, err := Collect(context.Background(), lister, Scope{State: StateOpen}, 10); err == nil {
		t.Fatal("Collect accepted a cursor that did not advance")
	}
}

func TestCollectStopsAtScanLimitAsIncomplete(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{
		"":  {items: []providers.WorkItem{item("1", "open"), item("2", "open")}, candidates: 3, next: "3"},
		"3": {items: []providers.WorkItem{item("4", "open")}, candidates: 3, next: "6"},
	}}
	items, coverage, err := Collect(context.Background(), lister, Scope{State: StateOpen}, 5)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := itemIDs(items); !slices.Equal(got, []string{"1", "2", "4"}) {
		t.Fatalf("items = %v", got)
	}
	if coverage.Status != StatusIncomplete || coverage.Reason != ReasonScanLimit || coverage.ExaminedCandidates != 6 {
		t.Fatalf("coverage = %+v, want incomplete scan-limit after 6 candidates", coverage)
	}
	if lister.requests[0].Limit != 5 || lister.requests[1].Limit != 2 {
		t.Fatalf("limits = %d,%d, want the remaining budget 5,2", lister.requests[0].Limit, lister.requests[1].Limit)
	}
}

func TestCollectDeadlineKeepsPartialItemsAsIncomplete(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{
		"":  {items: []providers.WorkItem{item("1", "open")}, candidates: 1, next: "1"},
		"1": {waitForDone: true},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	items, coverage, err := Collect(ctx, lister, Scope{State: StateOpen}, 10)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := itemIDs(items); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("items = %v, want the first page", got)
	}
	if coverage.Status != StatusIncomplete || coverage.Reason != ReasonDeadline {
		t.Fatalf("coverage = %+v, want incomplete deadline", coverage)
	}
}

func TestCollectCancellationIsAFailure(t *testing.T) {
	lister := &pagedLister{pages: map[string]listerPage{"": {waitForDone: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Collect(ctx, lister, Scope{State: StateOpen}, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect err = %v, want context.Canceled", err)
	}
}

func TestNewReportSeparatesIncompleteInput(t *testing.T) {
	predicate, err := fieldpredicate.Compile(`fields["type"] == "Task"`)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Labels: []string{"approved"}, FieldPredicate: predicate, State: StateAll}
	complete := Coverage{Status: StatusComplete}
	items := []providers.WorkItem{item("1", "open"), item("2", "closed")}

	report := NewReport(scope, ` fields["type"] == "Task" `, complete, items, []string{"1"})
	if report.Assessment != AssessmentComplete || len(report.ClaimedNotCompared) != 0 {
		t.Fatalf("report = %+v, want complete", report)
	}
	if !slices.Equal(report.Scope.ProviderNarrowed, []string{"labels"}) ||
		!slices.Equal(report.Scope.FilteredAfterRetrieval, []string{"fieldPredicate"}) ||
		report.Scope.FieldPredicate != `fields["type"] == "Task"` || report.Scope.State != StateAll {
		t.Fatalf("scope report = %+v", report.Scope)
	}

	report = NewReport(scope, "", complete, items, []string{"1", "9"})
	if report.Assessment != AssessmentIncomplete || !slices.Equal(report.ClaimedNotCompared, []string{"9"}) {
		t.Fatalf("report = %+v, want claimed 9 not compared", report)
	}

	report = NewReport(scope, "", Coverage{Status: StatusIncomplete, Reason: ReasonScanLimit}, nil, nil)
	if report.Assessment != AssessmentIncomplete {
		t.Fatalf("report = %+v, want incomplete for an incomplete collection with no candidates", report)
	}
}

func TestParseState(t *testing.T) {
	for raw, want := range map[string]string{"": StateOpen, "open": StateOpen, " ALL ": StateAll} {
		if got, err := ParseState(raw); err != nil || got != want {
			t.Fatalf("ParseState(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseState("closed"); err == nil {
		t.Fatal("ParseState(closed) succeeded; a comparison set must include the open selected items")
	}
}

var adoAfterIDPattern = regexp.MustCompile(`\[System\.Id\] > (\d+)`)

// TestCollectADONarrowsByTagsServerSide drives a real ADO provider against a
// fake WIQL endpoint that models server-side narrowing: only items whose tags
// contain the requested scope label are ever returned for hydration. Untagged
// project items must never be hydrated, which a project-wide read filtered
// client-side would do. The scope spans two WIQL pages.
func TestCollectADONarrowsByTagsServerSide(t *testing.T) {
	const total = 600
	tags := func(id int) string {
		switch id % 3 {
		case 0:
			return "unrelated"
		case 1:
			return "team-a"
		default:
			return "team-a-archive" // CONTAINS false positive, rechecked as a whole tag
		}
	}
	var mu sync.Mutex
	var queries []string
	hydrated := map[int]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode WIQL: %v", err)
			return
		}
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		after := 0
		if m := adoAfterIDPattern.FindStringSubmatch(body.Query); m != nil {
			after, _ = strconv.Atoi(m[1])
		}
		mu.Lock()
		queries = append(queries, body.Query)
		mu.Unlock()
		refs := []map[string]int{}
		for id := after + 1; id <= total && (top == 0 || len(refs) < top); id++ {
			if strings.Contains(body.Query, "[System.Tags] CONTAINS 'team-a'") && !strings.Contains(tags(id), "team-a") {
				continue
			}
			refs = append(refs, map[string]int{"id": id})
		}
		writeTestJSON(t, w, map[string]any{"workItems": refs})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []int `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode batch: %v", err)
			return
		}
		values := make([]map[string]any, 0, len(body.IDs))
		mu.Lock()
		for _, id := range body.IDs {
			hydrated[id] = true
			values = append(values, map[string]any{"id": id, "rev": 1, "fields": map[string]any{
				"System.Title": fmt.Sprintf("item %d", id), "System.State": "Done",
				"System.WorkItemType": "Task", "System.Tags": tags(id),
			}})
		}
		mu.Unlock()
		writeTestJSON(t, w, map[string]any{"value": values})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, map[string]any{"value": []map[string]string{
			{"name": "To Do", "category": "Proposed"}, {"name": "Done", "category": "Completed"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	scope := Scope{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "repo", Project: "project"},
		Labels:     []string{"team-a"},
		State:      StateAll,
	}
	items, coverage, err := Collect(context.Background(), provider, scope, 10000)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !coverage.Complete() || coverage.Pages < 2 {
		t.Fatalf("coverage = %+v, want a complete multi-page collection", coverage)
	}
	if len(items) != total/3 {
		t.Fatalf("collected %d items, want %d whole-tag matches", len(items), total/3)
	}
	for _, it := range items {
		if !slices.Contains(it.Labels, "team-a") || it.State != "closed" {
			t.Fatalf("item %s = labels %v state %q, want team-a completed history", it.ID, it.Labels, it.State)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, query := range queries {
		if !strings.Contains(query, "[System.Tags] CONTAINS 'team-a'") {
			t.Fatalf("WIQL %q does not narrow by the scope tag", query)
		}
	}
	for id := range hydrated {
		if tags(id) == "unrelated" {
			t.Fatalf("item %d outside the tag scope was hydrated; narrowing was not server-side", id)
		}
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
