package backlogscope

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/providers"
)

var (
	adoAfterIDPattern  = regexp.MustCompile(`\[System\.Id\] > (\d+)`)
	adoContainsPattern = regexp.MustCompile(`\[System\.Tags\] CONTAINS '((?:[^']|'')*)'`)
	adoEqualityPattern = regexp.MustCompile(`\[(System\.(?:AreaPath|WorkItemType))\] = '((?:[^']|'')*)'`)
)

type fakeADOItem struct {
	tags, itemType, area, state string
}

// fakeADO is an Azure DevOps work-item API that models WIQL server-side
// narrowing: tag CONTAINS is a case-insensitive substring match and field
// equality is case-insensitive, as in WIQL. It records every query, $top and
// hydrated id.
type fakeADO struct {
	t        *testing.T
	total    int
	attrs    func(id int) fakeADOItem
	mu       sync.Mutex
	queries  []string
	tops     []int
	hydrated map[int]bool
	// knownAreas, when set, are the project's area nodes; a WIQL area-path
	// equality naming any other node is rejected as ADO does (TF51011).
	knownAreas []string
}

func newFakeADO(t *testing.T, total int, attrs func(int) fakeADOItem) (*providers.ADOProvider, *fakeADO) {
	t.Helper()
	fake := &fakeADO{t: t, total: total, attrs: attrs, hydrated: map[int]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", fake.wiql)
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", fake.batch)
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, map[string]any{"value": []map[string]string{
			{"name": "To Do", "category": "Proposed"}, {"name": "Done", "category": "Completed"},
		}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL }), fake
}

func (f *fakeADO) wiql(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode WIQL: %v", err)
		return
	}
	top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
	after := 0
	if m := adoAfterIDPattern.FindStringSubmatch(body.Query); m != nil {
		after, _ = strconv.Atoi(m[1])
	}
	f.mu.Lock()
	f.queries = append(f.queries, body.Query)
	f.tops = append(f.tops, top)
	f.mu.Unlock()
	for _, m := range adoEqualityPattern.FindAllStringSubmatch(body.Query, -1) {
		area := strings.ReplaceAll(m[2], "''", "'")
		if m[1] == "System.AreaPath" && f.knownAreas != nil &&
			!slices.ContainsFunc(f.knownAreas, func(known string) bool { return strings.EqualFold(known, area) }) {
			http.Error(w, `{"message":"TF51011: The specified area path does not exist."}`, http.StatusBadRequest)
			return
		}
	}
	refs := []map[string]int{}
	for id := after + 1; id <= f.total && (top == 0 || len(refs) < top); id++ {
		if f.wiqlMatches(body.Query, f.attrs(id)) {
			refs = append(refs, map[string]int{"id": id})
		}
	}
	writeTestJSON(f.t, w, map[string]any{"workItems": refs})
}

func (f *fakeADO) wiqlMatches(query string, item fakeADOItem) bool {
	for _, m := range adoContainsPattern.FindAllStringSubmatch(query, -1) {
		if !strings.Contains(strings.ToLower(item.tags), strings.ToLower(strings.ReplaceAll(m[1], "''", "'"))) {
			return false
		}
	}
	for _, m := range adoEqualityPattern.FindAllStringSubmatch(query, -1) {
		value := map[string]string{"System.AreaPath": item.area, "System.WorkItemType": item.itemType}[m[1]]
		if !strings.EqualFold(value, strings.ReplaceAll(m[2], "''", "'")) {
			return false
		}
	}
	return true
}

func (f *fakeADO) batch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode batch: %v", err)
		return
	}
	values := make([]map[string]any, 0, len(body.IDs))
	f.mu.Lock()
	for _, id := range body.IDs {
		f.hydrated[id] = true
		item := f.attrs(id)
		values = append(values, map[string]any{"id": id, "rev": 1, "fields": map[string]any{
			"System.Title": fmt.Sprintf("item %d", id), "System.State": item.state,
			"System.WorkItemType": item.itemType, "System.AreaPath": item.area, "System.Tags": item.tags,
		}})
	}
	f.mu.Unlock()
	writeTestJSON(f.t, w, map[string]any{"value": values})
}

func adoScope(labels []string, predicate *fieldpredicate.Predicate, state string) Scope {
	return Scope{
		Repository:     providers.RepositoryRef{Provider: providers.ProviderADO, Name: "repo", Project: "project"},
		Labels:         labels,
		FieldPredicate: predicate,
		State:          state,
	}
}

// TestCollectADONarrowsByTagsServerSide drives a real ADO provider against a
// fake WIQL endpoint: only items whose tags contain the requested scope label
// are ever returned for hydration. Untagged project items must never be
// hydrated, which a project-wide read filtered client-side would do. The
// scope spans two WIQL pages.
func TestCollectADONarrowsByTagsServerSide(t *testing.T) {
	const total = 600
	tags := []string{"unrelated", "team-a", "team-a-archive"} // the last is a CONTAINS false positive
	provider, fake := newFakeADO(t, total, func(id int) fakeADOItem {
		return fakeADOItem{tags: tags[id%3], itemType: "Task", area: "project", state: "Done"}
	})
	items, coverage, err := Collect(context.Background(), provider, adoScope([]string{"team-a"}, nil, StateAll), 10000)
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
	for _, query := range fake.queries {
		if !strings.Contains(query, "[System.Tags] CONTAINS 'team-a'") {
			t.Fatalf("WIQL %q does not narrow by the scope tag", query)
		}
	}
	for id := range fake.hydrated {
		if tags[id%3] == "unrelated" {
			t.Fatalf("item %d outside the tag scope was hydrated; narrowing was not server-side", id)
		}
	}
}

// TestCollectADONarrowsByAreaAndTypeServerSide proves a scoped Task/area
// comparison narrows WIQL by the exact work-item-type and area-path
// equalities, so the broader tagged project backlog is never hydrated, while
// the case-sensitive predicate still rechecks WIQL's case-insensitive match.
func TestCollectADONarrowsByAreaAndTypeServerSide(t *testing.T) {
	const total = 600
	areas := []string{`project\Team A`, `project\Team B`, `PROJECT\team a`, `project\Team A\Sub`}
	types := []string{"Task", "Bug", "Task"}
	provider, fake := newFakeADO(t, total, func(id int) fakeADOItem {
		return fakeADOItem{tags: "approved", itemType: types[id%3], area: areas[id%4], state: "To Do"}
	})
	predicate, err := fieldpredicate.Compile(`fields["System.WorkItemType"] == "Task" && fields["System.AreaPath"] == "project\\Team A"`)
	if err != nil {
		t.Fatal(err)
	}
	scope := adoScope([]string{"approved"}, predicate, StateOpen)
	items, coverage, err := Collect(context.Background(), provider, scope, 10000)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !coverage.Complete() {
		t.Fatalf("coverage = %+v, want complete", coverage)
	}
	want := 0
	for id := 1; id <= total; id++ {
		if types[id%3] == "Task" && areas[id%4] == `project\Team A` {
			want++
		}
	}
	if len(items) != want {
		t.Fatalf("collected %d items, want %d exact Task/area matches", len(items), want)
	}
	for _, query := range fake.queries {
		if !strings.Contains(query, "[System.WorkItemType] = 'Task'") || !strings.Contains(query, `[System.AreaPath] = 'project\Team A'`) {
			t.Fatalf("WIQL %q does not narrow by the scope's work item type and area path", query)
		}
	}
	for id := range fake.hydrated {
		if !strings.EqualFold(types[id%3], "Task") || !strings.EqualFold(areas[id%4], `project\Team A`) {
			t.Fatalf("item %d (%s in %s) outside the type/area scope was hydrated", id, types[id%3], areas[id%4])
		}
	}
	report := NewReport(scope, "", coverage, items, nil)
	if !slices.Equal(report.Scope.ProviderNarrowed, []string{"labels", "fieldPredicate:System.WorkItemType", "fieldPredicate:System.AreaPath"}) ||
		!slices.Equal(report.Scope.FilteredAfterRetrieval, []string{"fieldPredicate"}) {
		t.Fatalf("scope report = %+v, want type/area provider narrowing with an exact recheck", report.Scope)
	}
}

// TestCollectADOUnknownAreaFallsBackToExactRecheck proves an area path ADO
// rejects in WIQL does not fail the read: the narrowed query is retried
// without the field clauses and the exact predicate matches nothing.
func TestCollectADOUnknownAreaFallsBackToExactRecheck(t *testing.T) {
	provider, fake := newFakeADO(t, 30, func(int) fakeADOItem {
		return fakeADOItem{tags: "approved", itemType: "Task", area: `project\Team A`, state: "To Do"}
	})
	fake.knownAreas = []string{"project", `project\Team A`}
	predicate, err := fieldpredicate.Compile(`fields["System.WorkItemType"] == "Task" && fields["System.AreaPath"] == "project\\Gone"`)
	if err != nil {
		t.Fatal(err)
	}
	items, coverage, err := Collect(context.Background(), provider, adoScope(nil, predicate, StateOpen), 100)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !coverage.Complete() || len(items) != 0 || coverage.ExaminedCandidates != 30 {
		t.Fatalf("items = %d coverage = %+v, want a complete empty collection over all 30 candidates", len(items), coverage)
	}
	if len(fake.queries) != 2 || !strings.Contains(fake.queries[0], `[System.AreaPath] = 'project\Gone'`) ||
		strings.Contains(fake.queries[1], "[System.AreaPath]") || strings.Contains(fake.queries[1], "[System.WorkItemType]") {
		t.Fatalf("queries = %q, want the narrowed query then an un-narrowed retry", fake.queries)
	}
}

// TestCollectADOBudgetBoundsOversizedWindow uses sparse whole-tag matches so
// the provider would otherwise read its oversized post-filter window: the
// budget must cap each WIQL $top and the examined total exactly.
func TestCollectADOBudgetBoundsOversizedWindow(t *testing.T) {
	provider, fake := newFakeADO(t, 1000, func(id int) fakeADOItem {
		tags := "team-a-archive"
		if id%10 == 0 {
			tags = "team-a"
		}
		return fakeADOItem{tags: tags, itemType: "Task", area: "project", state: "To Do"}
	})
	items, coverage, err := Collect(context.Background(), provider, adoScope([]string{"team-a"}, nil, StateOpen), 300)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if coverage.Status != StatusIncomplete || coverage.Reason != ReasonScanLimit || coverage.ExaminedCandidates != 300 {
		t.Fatalf("coverage = %+v, want incomplete scan-limit after exactly 300 candidates", coverage)
	}
	if len(items) != 30 || len(fake.hydrated) != 300 {
		t.Fatalf("items = %d hydrated = %d, want 30 matches from exactly 300 hydrated candidates", len(items), len(fake.hydrated))
	}
	if !slices.Equal(fake.tops, []int{250, 50}) {
		t.Fatalf("WIQL $top = %v, want the remaining budget [250 50]", fake.tops)
	}
}

// TestCollectGitHubBudgetBoundsPostFilterPage drives the GitHub provider with
// a field predicate, which makes it read full 100-record pages: the final
// page must be cut to the remaining budget.
func TestCollectGitHubBudgetBoundsPostFilterPage(t *testing.T) {
	const total = 500
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		pages = append(pages, fmt.Sprintf("%d/%d", page, perPage))
		issues := []map[string]any{}
		for n := (page-1)*perPage + 1; n <= min(page*perPage, total); n++ {
			issues = append(issues, map[string]any{"number": n, "title": fmt.Sprintf("item %d", n), "state": "open"})
		}
		writeTestJSON(t, w, issues)
	}))
	defer server.Close()
	provider := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) { p.BaseURL = server.URL })
	predicate, err := fieldpredicate.Compile(`fields["title"].endsWith("0")`)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{
		Repository:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "r"},
		FieldPredicate: predicate,
		State:          StateOpen,
	}
	items, coverage, err := Collect(context.Background(), provider, scope, 150)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if coverage.Status != StatusIncomplete || coverage.Reason != ReasonScanLimit || coverage.ExaminedCandidates != 150 || coverage.Pages != 2 {
		t.Fatalf("coverage = %+v, want incomplete scan-limit after exactly 150 candidates in 2 pages", coverage)
	}
	if len(items) != 15 || items[len(items)-1].ID != "150" {
		t.Fatalf("items = %v, want the 15 matches among the first 150 issues", itemIDs(items))
	}
	if !slices.Equal(pages, []string{"1/100", "2/100"}) {
		t.Fatalf("pages = %v, want two full-width pages", pages)
	}
}

// TestCollectGiteaBudgetCutsPageAndResumesInside proves the Gitea provider
// honours the budget inside a page and that its "page:skip" cursor resumes
// after the last inspected candidate.
func TestCollectGiteaBudgetCutsPageAndResumesInside(t *testing.T) {
	const total = 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		issues := []map[string]any{}
		for n := (page-1)*limit + 1; n <= min(page*limit, total); n++ {
			issues = append(issues, map[string]any{"id": n, "number": n, "title": fmt.Sprintf("item %d", n), "state": "open"})
		}
		w.Header().Set("x-total-count", strconv.Itoa(total))
		writeTestJSON(t, w, issues)
	}))
	defer server.Close()
	provider := providers.NewGiteaProvider(server.URL, "token")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "o", Name: "r"}
	items, coverage, err := Collect(context.Background(), provider, Scope{Repository: repo, State: StateOpen}, 70)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if coverage.Status != StatusIncomplete || coverage.ExaminedCandidates != 70 || len(items) != 70 || items[69].ID != "70" {
		t.Fatalf("coverage = %+v items = %d, want exactly the first 70 issues", coverage, len(items))
	}

	pageInfo := &providers.ListWorkItemsPageInfo{}
	resumed, err := provider.ListWorkItems(context.Background(), providers.ListWorkItemsRequest{
		Repository: repo, Limit: PageSize, MaxCandidates: 100, Cursor: "2:20", PageInfo: pageInfo,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumed) != 30 || resumed[0].ID != "71" || pageInfo.CandidateCount != 30 || !pageInfo.HasNext || pageInfo.NextCursor != "3" {
		t.Fatalf("resumed %v pageInfo = %+v, want issues 71-100 then page 3", itemIDs(resumed), pageInfo)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
