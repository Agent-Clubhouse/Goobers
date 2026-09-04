package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- ListWorkItems ---

func TestGiteaListWorkItemsFiltersAndMapsFields(t *testing.T) {
	var gotQuery map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/issues" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		gotQuery = map[string][]string(r.URL.Query())
		writeJSON(t, w, []map[string]interface{}{
			{
				"id": 123, "number": 7, "title": "Fix API", "body": "make it pass", "state": "open",
				"html_url": "https://gitea.test/acme/app/issues/7",
				"labels":   []map[string]interface{}{{"id": 1, "name": "route/backend"}},
			},
			{
				// A pull request masquerading as an issue must be excluded.
				"id": 124, "number": 8, "title": "a PR", "state": "open",
				"pull_request": map[string]interface{}{"merged": false},
			},
		})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:   RepositoryRef{Owner: "acme", Name: "app"},
		Labels:       []string{"route/backend"},
		State:        "open",
		UpdatedSince: &since,
	})
	if err != nil {
		t.Fatalf("ListWorkItems returned error: %v", err)
	}
	if got := gotQuery["type"]; len(got) != 1 || got[0] != "issues" {
		t.Fatalf("type query = %v, want [issues] (Gitea's issue list includes PRs otherwise)", got)
	}
	if got := gotQuery["labels"]; len(got) != 1 || got[0] != "route/backend" {
		t.Fatalf("labels query = %v", got)
	}
	if got := gotQuery["state"]; len(got) != 1 || got[0] != "open" {
		t.Fatalf("state query = %v", got)
	}
	if got := gotQuery["since"]; len(got) != 1 || got[0] != "2026-07-01T00:00:00Z" {
		t.Fatalf("since query = %v", got)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 (the pull-request-shaped issue must be excluded)", len(items))
	}
	if items[0].Provider != ProviderGitea || items[0].ID != "7" || !items[0].HasLabel("route/backend") {
		t.Fatalf("item = %#v", items[0])
	}
}

// TestGiteaListWorkItemsPageInfoFromTotalCountHeader proves caller-driven
// pagination (Page set) reads PageInfo from Gitea's x-total-count header.
func TestGiteaListWorkItemsPageInfoFromTotalCountHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("page"); got != "1" {
			t.Fatalf("page query = %q, want 1", got)
		}
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Fatalf("limit query = %q, want 2", got)
		}
		w.Header().Set("x-total-count", "5")
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 1, "title": "one", "state": "open"},
			{"id": 2, "number": 2, "title": "two", "state": "open"},
		})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	var pageInfo ListWorkItemsPageInfo
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Page:       1, Limit: 2, PageInfo: &pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if pageInfo.CandidateCount != 2 || !pageInfo.HasNext || pageInfo.NextCursor != "2" {
		t.Fatalf("pageInfo = %+v, want CandidateCount=2 HasNext=true NextCursor=2 (5 total, page 1 of size 2)", pageInfo)
	}
}

// TestGiteaListWorkItemsOldestFirstSortsAscending proves OldestFirst asks for
// a client-side ascending sort within the fetched window (Gitea has no
// server-side sort param for issues).
func TestGiteaListWorkItemsOldestFirstSortsAscending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Newest-first, as Gitea returns by default.
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 3, "title": "newest", "state": "open", "created_at": "2026-07-15T00:00:00Z"},
			{"id": 2, "number": 2, "title": "middle", "state": "open", "created_at": "2026-07-14T00:00:00Z"},
			{"id": 3, "number": 1, "title": "oldest", "state": "open", "created_at": "2026-07-13T00:00:00Z"},
		})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, OldestFirst: true,
	})
	if err != nil {
		t.Fatalf("ListWorkItems returned error: %v", err)
	}
	if len(items) != 3 || items[0].ID != "1" || items[1].ID != "2" || items[2].ID != "3" {
		t.Fatalf("items = %#v, want ascending creation order", items)
	}
}

// --- CreateWorkItem ---

// TestGiteaCreateWorkItemResolvesLabelIDsCreatingMissing proves labels are
// resolved to IDs (an existing label reused, a missing one created via
// POST /labels), and the created issue carries the resolved IDs.
func TestGiteaCreateWorkItemResolvesLabelIDsCreatingMissing(t *testing.T) {
	var created map[string]string
	var gotIssueBody map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/labels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, []map[string]interface{}{{"id": 1, "name": "route/backend"}})
		case http.MethodPost:
			decodeJSON(t, r, &created)
			writeJSON(t, w, map[string]interface{}{"id": 2, "name": created["name"]})
		default:
			t.Fatalf("unexpected labels method %s", r.Method)
		}
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		decodeJSON(t, r, &gotIssueBody)
		writeJSON(t, w, map[string]interface{}{
			"id": 999, "number": 11, "title": "New work", "state": "open",
			"labels": []map[string]interface{}{{"id": 1, "name": "route/backend"}, {"id": 2, "name": "goobers/status:claimed"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "New work",
		Labels:     []string{"route/backend"},
		Status:     WorkItemStatusClaimed,
	})
	if err != nil {
		t.Fatalf("CreateWorkItem returned error: %v", err)
	}
	if item.ID != "11" || item.Status != WorkItemStatusClaimed {
		t.Fatalf("item = %#v", item)
	}
	if created["name"] != "goobers/status:claimed" {
		t.Fatalf("created label = %#v, want the missing status label to be created", created)
	}
	labelIDs, _ := gotIssueBody["labels"].([]interface{})
	if len(labelIDs) != 2 {
		t.Fatalf("issue create body labels = %#v, want both resolved IDs [1 2]", gotIssueBody["labels"])
	}
}

// TestGiteaCreateWorkItemRunIDIdempotency proves a RunID re-pass finds the
// existing footer-matching issue instead of posting a duplicate.
func TestGiteaCreateWorkItemRunIDIdempotency(t *testing.T) {
	var posts int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, []map[string]interface{}{
				{
					"id": 5, "number": 5, "title": "Existing", "state": "open",
					"body": "---\ngoobers run-id: run-1",
				},
			})
		case http.MethodPost:
			posts++
			t.Fatalf("must not POST a duplicate when a run-id footer match exists")
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "New work", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem returned error: %v", err)
	}
	if item.ID != "5" {
		t.Fatalf("item = %#v, want the existing footer-matching issue #5", item)
	}
	if posts != 0 {
		t.Fatalf("posts = %d, want 0", posts)
	}
}

// --- shared issue mock for update/claim/release/blocker/transition/comments tests ---

// giteaIssueMock is a minimal in-memory Gitea issue backend for issue #7,
// with an ID-based label catalog (Gitea, unlike GitHub, addresses labels by
// numeric id, not name).
type giteaIssueMock struct {
	mu            sync.Mutex
	title         string
	body          string
	state         string
	assignees     []string
	labelCatalog  map[int64]string // id -> name, repo-wide
	labelIDs      []int64          // issue's current labels
	nextLabelID   int64
	comments      []map[string]interface{}
	nextCommentID int64
	userLogin     string
	dependencies  []map[string]interface{}
	timeline      []map[string]interface{}
	patchBody     map[string]interface{}
}

func newGiteaIssueMock() *giteaIssueMock {
	return &giteaIssueMock{
		title: "Fix API", body: "do it", state: "open",
		labelCatalog: map[int64]string{1: "route/backend"},
		labelIDs:     []int64{1},
		nextLabelID:  2,
		userLogin:    "goobers",
	}
}

func (m *giteaIssueMock) labelObjs() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(m.labelIDs))
	for _, id := range m.labelIDs {
		out = append(out, map[string]interface{}{"id": id, "name": m.labelCatalog[id]})
	}
	return out
}

func (m *giteaIssueMock) allLabelObjs() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(m.labelCatalog))
	for id, name := range m.labelCatalog {
		out = append(out, map[string]interface{}{"id": id, "name": name})
	}
	return out
}

func (m *giteaIssueMock) issueJSON() map[string]interface{} {
	assignees := make([]map[string]string, 0, len(m.assignees))
	for _, login := range m.assignees {
		assignees = append(assignees, map[string]string{"login": login})
	}
	return map[string]interface{}{
		"id": 123, "number": 7, "title": m.title, "body": m.body, "state": m.state,
		"html_url":  "https://gitea.test/acme/app/issues/7",
		"labels":    m.labelObjs(),
		"assignees": assignees,
	}
}

func (m *giteaIssueMock) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]string{"login": m.userLogin})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/labels", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, m.allLabelObjs())
		case http.MethodPost:
			var body map[string]string
			decodeJSON(t, r, &body)
			id := m.nextLabelID
			m.nextLabelID++
			m.labelCatalog[id] = body["name"]
			writeJSON(t, w, map[string]interface{}{"id": id, "name": body["name"]})
		default:
			t.Fatalf("unexpected labels method %s", r.Method)
		}
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected labels method %s", r.Method)
		}
		var body struct {
			Labels []int64 `json:"labels"`
		}
		decodeJSON(t, r, &body)
		seen := map[int64]bool{}
		for _, id := range m.labelIDs {
			seen[id] = true
		}
		for _, id := range body.Labels {
			if !seen[id] {
				m.labelIDs = append(m.labelIDs, id)
				seen[id] = true
			}
		}
		writeJSON(t, w, m.labelObjs())
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected label-delete method %s", r.Method)
		}
		idStr := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/acme/app/issues/7/labels/")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		next := make([]int64, 0, len(m.labelIDs))
		for _, l := range m.labelIDs {
			if l != id {
				next = append(next, l)
			}
		}
		m.labelIDs = next
		writeJSON(t, w, m.labelObjs())
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, m.comments)
		case http.MethodPost:
			var body map[string]string
			decodeJSON(t, r, &body)
			m.nextCommentID++
			c := map[string]interface{}{"id": m.nextCommentID, "body": body["body"], "user": map[string]string{"login": m.userLogin}}
			m.comments = append(m.comments, c)
			writeJSON(t, w, c)
		default:
			t.Fatalf("unexpected comments method %s", r.Method)
		}
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7/dependencies", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, m.dependencies)
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7/timeline", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, m.timeline)
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, m.issueJSON())
		case http.MethodPatch:
			var body map[string]interface{}
			decodeJSON(t, r, &body)
			m.patchBody = body
			if v, ok := body["title"].(string); ok {
				m.title = v
			}
			if v, ok := body["body"].(string); ok {
				m.body = v
			}
			if v, ok := body["state"].(string); ok {
				m.state = v
			}
			if values, ok := body["assignees"].([]interface{}); ok {
				m.assignees = m.assignees[:0]
				for _, v := range values {
					if login, ok := v.(string); ok {
						m.assignees = append(m.assignees, login)
					}
				}
			}
			writeJSON(t, w, m.issueJSON())
		default:
			t.Fatalf("unexpected issue method %s", r.Method)
		}
	})
	return mux
}

func newGiteaIssueProvider(t *testing.T, m *giteaIssueMock, opts ...func(*GiteaProvider)) (*GiteaProvider, RepositoryRef) {
	t.Helper()
	srv := httptest.NewServer(m.handler(t))
	t.Cleanup(srv.Close)
	all := append([]func(*GiteaProvider){func(p *GiteaProvider) {}}, opts...)
	provider := NewGiteaProvider(srv.URL, "token", all...)
	return provider, RepositoryRef{Owner: "acme", Name: "app"}
}

// --- UpdateWorkItem / UpdateWorkItemStatus ---

func TestGiteaUpdateWorkItemPatchesFieldsAndSwapsLabels(t *testing.T) {
	m := newGiteaIssueMock()
	p, repo := newGiteaIssueProvider(t, m)

	title := "New title"
	body := "New body"
	assignee := "mona"
	item, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository: repo, ID: "7", Title: &title, Body: &body, Assignee: &assignee,
		AddLabels: []string{"needs-review"}, RemoveLabels: []string{"route/backend"},
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem returned error: %v", err)
	}
	if item.Title != "New title" || item.Body != "New body" || item.Assignee != "mona" {
		t.Fatalf("item = %#v", item)
	}
	if item.HasLabel("route/backend") {
		t.Fatalf("expected route/backend removed: %v", item.Labels)
	}
	if !item.HasLabel("needs-review") {
		t.Fatalf("expected needs-review added: %v", item.Labels)
	}
	if _, ok := m.patchBody["labels"]; ok {
		t.Fatalf("label changes must go through the label sub-API, never a whole-set PATCH: %#v", m.patchBody)
	}
}

func TestGiteaUpdateWorkItemStatusSwapsStatusLabelOnly(t *testing.T) {
	m := newGiteaIssueMock()
	m.labelCatalog[2] = "goobers/status:claimed"
	m.labelIDs = append(m.labelIDs, 2)
	p, repo := newGiteaIssueProvider(t, m)

	item, err := p.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: repo, ID: "7", Status: WorkItemStatusInProgress,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus returned error: %v", err)
	}
	if item.Status != WorkItemStatusInProgress {
		t.Fatalf("Status = %q, want in-progress", item.Status)
	}
	if !item.HasLabel("route/backend") {
		t.Fatalf("non-status label must survive the swap: %v", item.Labels)
	}
	if item.HasLabel("goobers/status:claimed") {
		t.Fatalf("stale status label must be removed: %v", item.Labels)
	}
}

// --- ClaimWorkItem / ReleaseWorkItemClaim ---

func TestGiteaClaimWorkItemSingleWinnerUnderConcurrency(t *testing.T) {
	m := newGiteaIssueMock()
	p, repo := newGiteaIssueProvider(t, m)

	var wg sync.WaitGroup
	results := make([]ClaimResult, 2)
	errs := make([]error, 2)
	runIDs := []string{"run-A", "run-B"}
	wg.Add(2)
	for i := range runIDs {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.ClaimWorkItem(context.Background(), ClaimWorkItemRequest{
				Repository: repo, ID: "7", RunID: runIDs[i],
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("claim %s: %v", runIDs[i], err)
		}
	}
	winners := 0
	var winner ClaimResult
	for _, r := range results {
		if r.Claimed {
			winners++
			winner = r
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winner, got %d (%+v)", winners, results)
	}
	if results[0].ClaimedBy != results[1].ClaimedBy {
		t.Fatalf("runs disagree on winner: %q vs %q", results[0].ClaimedBy, results[1].ClaimedBy)
	}
	if winner.ClaimedBy != "run-A" && winner.ClaimedBy != "run-B" {
		t.Fatalf("winner not one of the racers: %q", winner.ClaimedBy)
	}
	if !winner.Item.HasLabel(LabelClaimed) {
		t.Fatalf("claimed label not applied to winner: %#v", winner.Item.Labels)
	}
}

func TestGiteaClaimWorkItemIdempotentAndAlreadyClaimed(t *testing.T) {
	m := newGiteaIssueMock()
	p, repo := newGiteaIssueProvider(t, m)
	ctx := context.Background()

	first, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	before := len(m.comments)
	again, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !again.Claimed || again.ClaimedBy != "run-A" {
		t.Fatalf("re-claim = %+v, %v", again, err)
	}
	if len(m.comments) != before {
		t.Fatalf("idempotent re-claim posted extra comment: %d -> %d", before, len(m.comments))
	}
	other, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-B"})
	if err != nil {
		t.Fatalf("loser claim error: %v", err)
	}
	if other.Claimed || other.ClaimedBy != "run-A" {
		t.Fatalf("expected run-B to lose to run-A, got %+v", other)
	}
}

func TestGiteaReleaseWorkItemClaimRemovesMarker(t *testing.T) {
	m := newGiteaIssueMock()
	p, repo := newGiteaIssueProvider(t, m)
	ctx := context.Background()

	first, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	released, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil {
		t.Fatalf("release claim: %v", err)
	}
	if released.HasLabel(LabelClaimed) {
		t.Fatalf("released item still has %q: %v", LabelClaimed, released.Labels)
	}
	commentCount := len(m.comments)
	if _, err := p.ReleaseWorkItemClaim(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"}); err != nil {
		t.Fatalf("retry release claim: %v", err)
	}
	if len(m.comments) != commentCount {
		t.Fatalf("retry release posted duplicate comment: %d -> %d", commentCount, len(m.comments))
	}
	winner, claimed, err := claimWinner(ctx, p, p.BaseURL, repo, "7")
	if err != nil {
		t.Fatalf("claimWinner after release: %v", err)
	}
	if claimed {
		t.Fatalf("claimWinner after release = %q, want no active claim", winner)
	}
}

// --- HasOpenWorkItemBlocker ---

func TestGiteaHasOpenWorkItemBlocker(t *testing.T) {
	m := newGiteaIssueMock()
	p, repo := newGiteaIssueProvider(t, m)

	m.dependencies = []map[string]interface{}{
		{"id": 1, "number": 3, "state": "closed"},
	}
	open, err := p.HasOpenWorkItemBlocker(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("HasOpenWorkItemBlocker returned error: %v", err)
	}
	if open {
		t.Fatal("open = true, want false (only a closed dependency)")
	}

	m.dependencies = append(m.dependencies, map[string]interface{}{"id": 2, "number": 4, "state": "open"})
	open, err = p.HasOpenWorkItemBlocker(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("HasOpenWorkItemBlocker returned error: %v", err)
	}
	if !open {
		t.Fatal("open = false, want true (an open dependency is present)")
	}
}

// --- ListWorkItemLabelTransitionsForItem ---

func TestGiteaListWorkItemLabelTransitionsForItem(t *testing.T) {
	m := newGiteaIssueMock()
	p, repo := newGiteaIssueProvider(t, m)

	m.timeline = []map[string]interface{}{
		{"id": 1, "type": "label", "body": "1", "label": map[string]interface{}{"name": "goobers:ready"}, "created_at": "2026-07-10T00:00:00Z"},
		{"id": 2, "type": "comment", "body": "unrelated"},
		{"id": 3, "type": "label", "body": "", "label": map[string]interface{}{"name": "goobers:ready"}, "created_at": "2026-07-11T00:00:00Z"},
		{"id": 4, "type": "label", "body": "1", "label": map[string]interface{}{"name": "other"}, "created_at": "2026-07-12T00:00:00Z"},
	}
	transitions, err := p.ListWorkItemLabelTransitionsForItem(context.Background(), repo, "7", "goobers:ready")
	if err != nil {
		t.Fatalf("ListWorkItemLabelTransitionsForItem returned error: %v", err)
	}
	if len(transitions) != 2 {
		t.Fatalf("transitions = %#v, want 2 (filtered to goobers:ready)", transitions)
	}
	if !transitions[0].Added || transitions[1].Added {
		t.Fatalf("transitions = %#v, want [added, removed] in chronological order", transitions)
	}
}

// --- ListComments ---

func TestGiteaListComments(t *testing.T) {
	m := newGiteaIssueMock()
	created := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	m.comments = []map[string]interface{}{
		{"id": 1, "body": "first", "user": map[string]string{"login": "dependabot"}, "created_at": created, "html_url": "c1"},
	}
	p, repo := newGiteaIssueProvider(t, m)
	comments, err := p.ListComments(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "1" || comments[0].Author != "dependabot" || comments[0].Body != "first" {
		t.Fatalf("unexpected comments: %#v", comments)
	}
	if comments[0].CreatedAt == nil || !comments[0].CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", comments[0].CreatedAt, created)
	}
}

func TestGiteaDecompositionMarkerAndCommentMutations(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=child -->"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "all" || r.URL.Query().Get("type") != "issues" {
			t.Fatalf("query = %q, want authoritative all-issues listing", r.URL.RawQuery)
		}
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 1, "title": "match", "body": "body\n" + marker, "state": "open"},
			{"id": 2, "number": 2, "title": "substring", "body": "prefix " + marker, "state": "open"},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body map[string]string
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]interface{}{"id": 9, "body": body["body"]})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	items, err := provider.FindWorkItemsByMarker(context.Background(), repo, marker)
	if err != nil {
		t.Fatalf("FindWorkItemsByMarker: %v", err)
	}
	if len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("items = %#v, want exact marker match #1", items)
	}
	comment, err := provider.CreateWorkItemComment(context.Background(), repo, "7", "prepared")
	if err != nil {
		t.Fatalf("CreateWorkItemComment: %v", err)
	}
	if comment.ID != "9" || comment.Body != "prepared" {
		t.Fatalf("comment = %#v", comment)
	}
}
