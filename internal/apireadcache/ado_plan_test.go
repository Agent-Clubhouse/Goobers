package apireadcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type adoPlanFixture struct {
	mu      sync.Mutex
	calls   map[string]int
	queries []string
	batches [][]int
	before  func(*http.Request) error
}

func (f *adoPlanFixture) Do(req *http.Request) (*http.Response, error) {
	kind := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[kind]++
	f.mu.Unlock()
	if f.before != nil {
		if err := f.before(req); err != nil {
			return nil, err
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	body := ""
	switch kind {
	case "wiql":
		var query struct{ Query string }
		if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.queries = append(f.queries, query.Query)
		f.mu.Unlock()
		body = `{"workItems":[{"id":1},{"id":2},{"id":3}]}`
	case "workitemsbatch":
		var batch struct{ IDs []int }
		if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.batches = append(f.batches, batch.IDs)
		f.mu.Unlock()
		// Deliberately reverse order, omit item 2, and include an unrequested ID.
		body = `{"value":[` + adoPlanItem(3) + `,null,` + adoPlanItem(1) + `,` + adoPlanItem(99) + `]}`
	case "states":
		body = `{"value":[{"name":"Active","category":"InProgress"}]}`
	default:
		return nil, fmt.Errorf("unexpected fixture request: %s", req.URL.Path)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func adoPlanItem(id int) string {
	return fmt.Sprintf(`{"id":%d,"rev":9,"fields":{"System.Title":"Item","System.TeamProject":"project","System.WorkItemType":"Task","System.State":"Active","System.Tags":"scope; approved"}}`, id)
}
func (f *adoPlanFixture) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls["wiql"], f.calls["workitemsbatch"]
}
func adoPlanProvider(dir, snapshot string, scope Scope, token string, inner providers.HTTPClient) *providers.ADOProvider {
	return providers.NewADOProvider("org", "project", token, func(p *providers.ADOProvider) {
		p.Client = ScopedClient(dir, snapshot, scope, providers.ProviderADO, inner)
	})
}
func adoPlanRead(ctx context.Context, p *providers.ADOProvider) ([]providers.WorkItem, providers.ListWorkItemsPageInfo, error) {
	info := providers.ListWorkItemsPageInfo{}
	items, err := p.ListWorkItems(ctx, providers.ListWorkItemsRequest{Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project"}, State: "all", Limit: 3, OldestFirst: true, PageInfo: &info})
	return items, info, err
}
func assertADOPlanItems(t *testing.T, items []providers.WorkItem, info providers.ListWorkItemsPageInfo, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if !slices.Equal(ids, []string{"1", "3"}) || info.CandidateCount != 3 || !info.HasNext || info.NextCursor != "3" {
		t.Fatalf("changed hydration order, omissions or cursor: %v %+v", ids, info)
	}
}

func TestADOPlanConcurrentProviderListsShareExactSnapshotAndBatchMembership(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "automation:backlog", Generation: "generation-a"}
	fixture := &adoPlanFixture{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			items, info, err := adoPlanRead(t.Context(), adoPlanProvider(dir, "evaluation", scope, "token", fixture))
			assertADOPlanItems(t, items, info, err)
		})
	}
	wg.Wait()
	if queries, batches := fixture.counts(); queries != 1 || batches != 1 {
		t.Fatalf("8 consumers made %d queries, %d hydration batches", queries, batches)
	}
	if !slices.Equal(fixture.batches[0], []int{1, 2, 3}) || !strings.Contains(fixture.queries[0], "[System.TeamProject] = @project") || !strings.HasSuffix(fixture.queries[0], "ORDER BY [System.Id] ASC") {
		t.Fatalf("query or membership changed: %+v %+v", fixture.queries, fixture.batches)
	}
}

func TestADOPlanPartitionsScopeCredentialsRepresentationsAndRefresh(t *testing.T) {
	dir := t.TempDir()
	fixture := &adoPlanFixture{}
	scope := Scope{Gaggle: "team", Binding: "interactive:planning", Generation: "generation-a"}
	base := adoPlanProvider(dir, "evaluation", scope, "token", fixture)
	read := func(p *providers.ADOProvider) {
		t.Helper()
		items, info, err := adoPlanRead(t.Context(), p)
		assertADOPlanItems(t, items, info, err)
	}
	read(base)
	read(base)
	for _, change := range []Scope{{Gaggle: "other", Binding: scope.Binding, Generation: scope.Generation}, {Gaggle: scope.Gaggle, Binding: "interactive:other-credential", Generation: scope.Generation}, {Gaggle: scope.Gaggle, Binding: scope.Binding, Generation: "generation-b"}} {
		read(adoPlanProvider(dir, "evaluation", change, "token", fixture))
	}
	read(adoPlanProvider(dir, "evaluation", scope, "rotated-token", fixture))
	representation := adoPlanProvider(dir, "evaluation", scope, "token", fixture)
	inner := representation.Client
	representation.Client = scopedTransport(func(req *http.Request) (*http.Response, error) {
		req.Header.Set("Accept", "application/vnd.different+json")
		return inner.Do(req)
	})
	read(representation)
	if queries, batches := fixture.counts(); queries != 6 || batches != 6 {
		t.Fatalf("cross-scope reuse: %d queries, %d batches", queries, batches)
	}
	if err := InvalidateScopedSnapshot(dir, "evaluation", scope); err != nil {
		t.Fatal(err)
	}
	read(base) // Same wrapper must observe invalidation, not its local memory.
	if queries, batches := fixture.counts(); queries != 7 || batches != 7 {
		t.Fatalf("explicit refresh reused snapshot: %d %d", queries, batches)
	}
	for range 2 {
		read(adoPlanProvider(dir, "", scope, "token", fixture))
	}
	if queries, batches := fixture.counts(); queries != 9 || batches != 9 {
		t.Fatalf("sequential interactive reads reused completed response: %d %d", queries, batches)
	}
}

func TestADOPlanInteractiveOverlapSharesButLaterRefreshIsLive(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "interactive:planning", Generation: "generation-a"}
	var enteredWIQL, enteredBatch atomic.Int32
	fixture := &adoPlanFixture{before: func(req *http.Request) error {
		var entered *atomic.Int32
		switch {
		case strings.HasSuffix(req.URL.Path, "/wiql"):
			entered = &enteredWIQL
		case strings.HasSuffix(req.URL.Path, "/workitemsbatch"):
			entered = &enteredBatch
		default:
			return nil
		}
		return waitADOPlanCondition(req.Context(), func() bool { return entered.Load() >= 8 })
	}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p := adoPlanProvider(dir, "", scope, "token", fixture)
			inner := p.Client
			p.Client = scopedTransport(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/wiql") {
					enteredWIQL.Add(1)
				} else if strings.HasSuffix(req.URL.Path, "/workitemsbatch") {
					enteredBatch.Add(1)
				}
				return inner.Do(req)
			})
			items, info, err := adoPlanRead(t.Context(), p)
			assertADOPlanItems(t, items, info, err)
		})
	}
	wg.Wait()
	if queries, batches := fixture.counts(); queries != 1 || batches != 1 {
		t.Fatalf("overlap did not share: %d %d", queries, batches)
	}
	items, info, err := adoPlanRead(t.Context(), adoPlanProvider(dir, "", scope, "token", fixture))
	assertADOPlanItems(t, items, info, err)
	if queries, batches := fixture.counts(); queries != 2 || batches != 2 {
		t.Fatalf("refresh was not live: %d %d", queries, batches)
	}
}

func TestADOPlanQueryShapeAndBatchSubsetsNeverBroadenMembership(t *testing.T) {
	fixture := &adoPlanFixture{}
	p := adoPlanProvider(t.TempDir(), "evaluation", Scope{Gaggle: "team", Binding: "backlog", Generation: "a"}, "token", fixture)
	items, info, err := adoPlanRead(t.Context(), p)
	assertADOPlanItems(t, items, info, err)
	for range 2 {
		info := providers.ListWorkItemsPageInfo{}
		items, err := p.ListWorkItems(t.Context(), providers.ListWorkItemsRequest{Repository: providers.RepositoryRef{Project: "project"}, State: "all", Limit: 1, PageInfo: &info})
		if err != nil || len(items) != 1 || items[0].ID != "1" {
			t.Fatalf("subset hydration changed: %v %v", items, err)
		}
	}
	if queries, batches := fixture.counts(); queries != 2 || batches != 2 {
		t.Fatalf("different read plans shared a response: %d %d", queries, batches)
	}
	if !slices.Equal(fixture.batches[1], []int{1}) {
		t.Fatalf("subset request widened: %v", fixture.batches)
	}
	var query struct{ Query string }
	p.Client = ScopedClient(t.TempDir(), "evaluation", Scope{Gaggle: "team", Binding: "backlog", Generation: "a"}, providers.ProviderADO, scopedTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/org/Separate%20Backlog/_apis/wit/wiql" && req.URL.Path != "/org/Separate Backlog/_apis/wit/wiql" {
			t.Fatalf("wrong backlog routing: %s", req.URL)
		}
		if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"workItems":[]}`))}, nil
	}))
	_, err = p.ListWorkItems(t.Context(), providers.ListWorkItemsRequest{Repository: providers.RepositoryRef{Project: "Separate Backlog"}, State: "Active", Assignee: "ada'o", Labels: []string{"team's scope"}, Cursor: "42", Limit: 3, PageInfo: &providers.ListWorkItemsPageInfo{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"[System.TeamProject] = @project", "[System.State] = 'Active'", "[System.AssignedTo] = 'ada''o'", "[System.Tags] CONTAINS 'team''s scope'", "[System.Id] > 42", "ORDER BY [System.Id] ASC"} {
		if !strings.Contains(query.Query, part) {
			t.Fatalf("read plan changed routing/filter/cursor: %s missing %s", query.Query, part)
		}
	}
}

func waitADOPlanCondition(ctx context.Context, ready func() bool) error {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("fixture wait timed out")
		case <-ticker.C:
		}
	}
	// Let the last caller enter the cache before the transport finishes.
	return nil
}
