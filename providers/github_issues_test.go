package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/labelpredicate"
)

// recordingRecorder captures external-ref mutations for assertions.
type recordingRecorder struct {
	mu   sync.Mutex
	refs []ExternalRef
}

func (r *recordingRecorder) RecordExternalRef(_ context.Context, ref ExternalRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
}

func (r *recordingRecorder) last() (ExternalRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.refs) == 0 {
		return ExternalRef{}, false
	}
	return r.refs[len(r.refs)-1], true
}

// recordingObserver captures rate-limit events.
type recordingObserver struct {
	mu     sync.Mutex
	events []RateLimitEvent
}

type recordingQuotaObserver struct {
	mu           sync.Mutex
	observations []QuotaObservation
}

func (o *recordingQuotaObserver) ObserveQuota(_ context.Context, observation QuotaObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observations = append(o.observations, observation)
}

func (o *recordingQuotaObserver) last() (QuotaObservation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.observations) == 0 {
		return QuotaObservation{}, false
	}
	return o.observations[len(o.observations)-1], true
}

func (o *recordingObserver) ObserveRateLimit(_ context.Context, ev RateLimitEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
}

func (o *recordingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

type staticTokenSource struct {
	token string
	calls int
}

func (s *staticTokenSource) Token(context.Context) (string, error) {
	s.calls++
	return s.token, nil
}

func TestGitHubProviderObservesQuotaHeaders(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "17")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(resetAt.Unix()))
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()

	observer := &recordingQuotaObserver{}
	provider := NewGitHubProvider("token", WithQuotaObserver(observer), func(p *GitHubProvider) {
		p.BaseURL = server.URL
	})
	_, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "web"},
		State:      "open",
		Limit:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, ok := observer.last()
	if !ok {
		t.Fatal("quota observer received no observation")
	}
	if observation.Provider != ProviderGitHub || !observation.Known || observation.Cached ||
		observation.Remaining != 17 || !observation.Reset.Equal(resetAt) {
		t.Fatalf("quota observation = %+v, want GitHub remaining=17 reset=%s", observation, resetAt)
	}
}

// issueMock is a minimal in-memory GitHub issue backend covering the endpoints the
// issue operations touch: read issue, list/post comments, add/remove labels, patch.
type issueMock struct {
	mu        sync.Mutex
	title     string
	body      string
	state     string
	labels    []string
	assignees []string
	milestone int
	comments  []map[string]interface{}
	nextID    int64
	authSeen  string
	userLogin string
	patchBody map[string]interface{}
	children  []map[string]interface{}
}

func newIssueMock() *issueMock {
	return &issueMock{title: "Fix API", body: "do it", state: "open", labels: []string{"route/backend"}, userLogin: "goobers"}
}

func (m *issueMock) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected user method %s", r.Method)
		}
		writeJSON(t, w, map[string]string{"login": m.userLogin})
	})
	mux.HandleFunc("/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, m.comments)
		case http.MethodPost:
			var body map[string]string
			decodeJSON(t, r, &body)
			m.nextID++
			c := map[string]interface{}{"id": m.nextID, "body": body["body"], "user": map[string]string{"login": "goobers"}}
			m.comments = append(m.comments, c)
			writeJSON(t, w, c)
		default:
			t.Fatalf("unexpected comments method %s", r.Method)
		}
	})
	mux.HandleFunc("/repos/acme/app/issues/7/sub_issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected sub-issues method %s", r.Method)
		}
		writeJSON(t, w, m.children)
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected labels method %s", r.Method)
		}
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeJSON(t, r, &body)
		m.labels = uniqueStrings(append(m.labels, body.Labels...))
		writeJSON(t, w, labelObjects(m.labels))
	})
	// DELETE /labels/{name}
	mux.HandleFunc("/repos/acme/app/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected label-delete method %s", r.Method)
		}
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/app/issues/7/labels/")
		next := make([]string, 0, len(m.labels))
		found := false
		for _, l := range m.labels {
			if l == name {
				found = true
				continue
			}
			next = append(next, l)
		}
		if !found {
			http.Error(w, "label not found", http.StatusNotFound)
			return
		}
		m.labels = next
		writeJSON(t, w, labelObjects(m.labels))
	})
	// PATCH/DELETE /repos/acme/app/issues/comments/{id}: comment IDs are
	// repo-scoped, not nested under the issue number.
	mux.HandleFunc("/repos/acme/app/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/repos/acme/app/issues/comments/")
		for i, c := range m.comments {
			if fmt.Sprint(c["id"]) != id {
				continue
			}
			switch r.Method {
			case http.MethodPatch:
				var body map[string]string
				decodeJSON(t, r, &body)
				m.comments[i]["body"] = body["body"]
				writeJSON(t, w, m.comments[i])
				return
			case http.MethodDelete:
				m.comments = append(m.comments[:i], m.comments[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				t.Fatalf("unexpected comment mutation method %s", r.Method)
			}
		}
		http.Error(w, "comment not found", http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.authSeen = r.Header.Get("Authorization")
		if r.Method == http.MethodPatch {
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
			if v, ok := body["milestone"].(float64); ok {
				m.milestone = int(v)
			}
			if values, ok := body["assignees"].([]interface{}); ok {
				m.assignees = make([]string, 0, len(values))
				for _, value := range values {
					if login, ok := value.(string); ok {
						m.assignees = append(m.assignees, login)
					}
				}
			}
		}
		writeJSON(t, w, m.issueJSON())
	})
	return mux
}

func (m *issueMock) issueJSON() map[string]interface{} {
	out := map[string]interface{}{
		"id": 123, "number": 7, "title": m.title, "body": m.body, "state": m.state,
		"html_url": "https://github.com/acme/app/issues/7",
		"labels":   labelObjects(m.labels),
	}
	assignees := make([]map[string]string, 0, len(m.assignees))
	for _, login := range m.assignees {
		assignees = append(assignees, map[string]string{"login": login})
	}
	out["assignees"] = assignees
	if m.milestone > 0 {
		out["milestone"] = map[string]interface{}{
			"id": m.milestone, "number": m.milestone, "title": fmt.Sprintf("Milestone %d", m.milestone),
		}
	}
	return out
}

func labelObjects(labels []string) []map[string]string {
	out := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func newIssueProvider(t *testing.T, m *issueMock, opts ...func(*GitHubProvider)) (*GitHubProvider, RepositoryRef) {
	t.Helper()
	srv := httptest.NewServer(m.handler(t))
	t.Cleanup(srv.Close)
	all := append([]func(*GitHubProvider){func(p *GitHubProvider) { p.BaseURL = srv.URL }}, opts...)
	return NewGitHubProvider("token", all...), RepositoryRef{Owner: "acme", Name: "app"}
}

func TestGitHubListComments(t *testing.T) {
	m := newIssueMock()
	created := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	m.comments = []map[string]interface{}{
		{"id": 1, "body": "first", "user": map[string]string{"login": "dependabot[bot]", "type": "Bot"}, "created_at": created, "html_url": "c1"},
	}

	p, repo := newIssueProvider(t, m)
	comments, err := p.ListComments(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "1" || comments[0].Author != "dependabot[bot]" ||
		comments[0].AuthorType != "Bot" || comments[0].Body != "first" {
		t.Fatalf("unexpected comments: %#v", comments)
	}
	if comments[0].CreatedAt == nil || !comments[0].CreatedAt.Equal(created) {
		t.Fatalf("expected created_at preserved, got %#v", comments[0].CreatedAt)
	}
}

func TestGitHubFindWorkItemsByMarkerUsesExactAuthoritativeListing(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=Y2hpbGQ -->"
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "all" {
			t.Fatalf("state = %q, want all", r.URL.Query().Get("state"))
		}
		writeJSON(t, w, []map[string]interface{}{
			{"id": 101, "number": 11, "title": "exact", "body": "body\n\n" + marker, "state": "open"},
			{"id": 102, "number": 12, "title": "substring", "body": "prefix " + marker, "state": "open"},
			{"id": 103, "number": 13, "title": "pr", "body": marker, "state": "open", "pull_request": map[string]string{"url": "pull"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	items, err := provider.FindWorkItemsByMarker(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, marker)
	if err != nil {
		t.Fatalf("FindWorkItemsByMarker: %v", err)
	}
	if len(items) != 1 || items[0].ID != "11" {
		t.Fatalf("items = %#v, want exact issue 11 only", items)
	}
}

func TestGitHubCreateWorkItemCommentReturnsIdentity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		writeJSON(t, w, map[string]interface{}{
			"id": 81, "body": "prepared", "html_url": "https://github.test/comments/81",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	comment, err := provider.CreateWorkItemComment(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7", "prepared")
	if err != nil {
		t.Fatalf("CreateWorkItemComment: %v", err)
	}
	if comment.ID != "81" || comment.Body != "prepared" {
		t.Fatalf("comment = %#v", comment)
	}
}

func TestGitHubAttachWorkItemChildGuardsRevisions(t *testing.T) {
	const revision = "2026-08-03T12:00:00Z"
	var posts int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 70, "number": 7, "title": "parent", "state": "open", "updated_at": revision,
		})
	})
	mux.HandleFunc("/repos/acme/app/issues/8", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 80, "number": 8, "title": "child", "state": "open", "updated_at": revision,
		})
	})
	mux.HandleFunc("/repos/acme/app/issues/7/sub_issues", func(w http.ResponseWriter, r *http.Request) {
		posts++
		var body map[string]int64
		decodeJSON(t, r, &body)
		if body["sub_issue_id"] != 80 {
			t.Fatalf("sub_issue_id = %d, want 80", body["sub_issue_id"])
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	if err := provider.AttachWorkItemChild(context.Background(), AttachWorkItemChildRequest{
		Repository: repo, ParentID: "7", ChildID: "8",
		ExpectedParentRevision: revision, ExpectedChildRevision: revision,
	}); err != nil {
		t.Fatalf("AttachWorkItemChild: %v", err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}

	err := provider.AttachWorkItemChild(context.Background(), AttachWorkItemChildRequest{
		Repository: repo, ParentID: "7", ChildID: "8",
		ExpectedParentRevision: "stale", ExpectedChildRevision: revision,
	})
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	if posts != 1 {
		t.Fatalf("stale revision caused a mutation; posts = %d", posts)
	}
}

func TestGitHubAttachWorkItemBlockerGuardsRevisionsAndLists(t *testing.T) {
	const revision = "2026-08-03T12:00:00Z"
	var posts int
	mux := http.NewServeMux()
	for id, databaseID := range map[string]int{"8": 80, "9": 90} {
		id, databaseID := id, databaseID
		number, err := strconv.Atoi(id)
		if err != nil {
			t.Fatal(err)
		}
		mux.HandleFunc("/repos/acme/app/issues/"+id, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]interface{}{
				"id": databaseID, "number": number, "title": "issue " + id, "state": "open", "updated_at": revision,
			})
		})
	}
	mux.HandleFunc("/repos/acme/app/issues/8/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, []map[string]interface{}{{
				"id": 90, "number": 9, "title": "blocker", "state": "open", "updated_at": revision,
			}})
			return
		}
		posts++
		var body map[string]int64
		decodeJSON(t, r, &body)
		if body["issue_id"] != 90 {
			t.Fatalf("issue_id = %d, want 90", body["issue_id"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	blockers, err := provider.ListWorkItemBlockers(context.Background(), repo, "8")
	if err != nil || len(blockers) != 1 || blockers[0].ID != "9" {
		t.Fatalf("ListWorkItemBlockers = %#v, %v", blockers, err)
	}
	if err := provider.AttachWorkItemBlocker(context.Background(), AttachWorkItemBlockerRequest{
		Repository: repo, ItemID: "8", BlockerID: "9",
		ExpectedItemRevision: revision, ExpectedBlockerRevision: revision,
	}); err != nil {
		t.Fatalf("AttachWorkItemBlocker: %v", err)
	}
	err = provider.AttachWorkItemBlocker(context.Background(), AttachWorkItemBlockerRequest{
		Repository: repo, ItemID: "8", BlockerID: "9",
		ExpectedItemRevision: "stale", ExpectedBlockerRevision: revision,
	})
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}
}

func TestGitHubListWorkItemLabelTransitionsPaginatesAndFilters(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/issues/events" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Fatalf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf("<%s%s?page=2&per_page=100>; rel=\"next\"", srv.URL, r.URL.Path))
			writeJSON(t, w, []map[string]any{
				{"id": 10, "event": "labeled", "created_at": "2026-07-20T10:00:00Z", "label": map[string]string{"name": LabelReady}, "issue": map[string]any{"number": 7}},
				{"id": 11, "event": "labeled", "created_at": "2026-07-20T11:00:00Z", "label": map[string]string{"name": "other"}, "issue": map[string]any{"number": 7}},
				{"id": 12, "event": "labeled", "created_at": "2026-07-20T12:00:00Z", "label": map[string]string{"name": LabelReady}, "issue": map[string]any{"number": 8, "pull_request": map[string]string{"url": "pr"}}},
			})
			return
		}
		writeJSON(t, w, []map[string]any{
			{"id": 13, "event": "unlabeled", "created_at": "2026-07-21T10:00:00Z", "label": map[string]string{"name": LabelReady}, "issue": map[string]any{"number": 7}},
		})
	}))
	t.Cleanup(srv.Close)
	p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL })

	got, err := p.ListWorkItemLabelTransitions(
		context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"},
		LabelReady,
	)
	if err != nil {
		t.Fatalf("ListWorkItemLabelTransitions: %v", err)
	}
	if len(got) != 2 || got[0].EventID != 10 || !got[0].Added ||
		got[1].EventID != 13 || got[1].Added || got[1].ItemID != "7" {
		t.Fatalf("transitions = %#v", got)
	}
}

func TestGitHubAuthenticatedLogin(t *testing.T) {
	m := newIssueMock()
	p, _ := newIssueProvider(t, m)

	login, err := p.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "goobers" {
		t.Fatalf("login = %q, want %q", login, "goobers")
	}
}

func TestGitHubAuthenticatedLoginRequiresLogin(t *testing.T) {
	m := newIssueMock()
	m.userLogin = ""
	p, _ := newIssueProvider(t, m)

	if _, err := p.AuthenticatedLogin(context.Background()); err == nil {
		t.Fatal("AuthenticatedLogin with an empty login: err = nil, want an error")
	}
}

func TestGitHubListWorkItemChildren(t *testing.T) {
	m := newIssueMock()
	m.children = []map[string]interface{}{
		{"id": 124, "number": 8, "title": "Child", "state": "open", "html_url": "https://github.com/acme/app/issues/8"},
	}
	p, repo := newIssueProvider(t, m)

	children, err := p.ListWorkItemChildren(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("ListWorkItemChildren: %v", err)
	}
	if len(children) != 1 || children[0].ID != "8" || children[0].State != "open" {
		t.Fatalf("children = %#v, want open issue 8", children)
	}
}

func TestGitHubUpdateWorkItemEditsLabelsCloseComment(t *testing.T) {
	m := newIssueMock()
	rec := &recordingRecorder{}
	p, repo := newIssueProvider(t, m, WithMutationRecorder(rec))
	before, err := p.GetWorkItem(context.Background(), repo, "7")
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "Fix API v2"
	item, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository:       repo,
		ID:               "7",
		ExpectedRevision: before.Revision,
		Title:            &newTitle,
		AddLabels:        []string{LabelReady},
		RemoveLabels:     []string{"route/backend"},
		State:            "closed",
		Comment:          "done and dusted",
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if item.Title != "Fix API v2" || item.State != "closed" {
		t.Fatalf("unexpected final item: %#v", item)
	}
	if !item.HasLabel(LabelReady) || item.HasLabel("route/backend") {
		t.Fatalf("labels not applied: %#v", item.Labels)
	}
	if got := len(m.comments); got != 1 {
		t.Fatalf("expected 1 comment posted, got %d", got)
	}
	// External-ref mutation recorded with before/after digests for each field.
	ref, ok := rec.last()
	if !ok {
		t.Fatal("expected an external-ref mutation to be recorded")
	}
	if ref.Ref != "acme/app#7" || ref.Operation != "close" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
	for _, field := range []string{"title", "state", "labels", "comment"} {
		fd, ok := ref.Fields[field]
		if !ok {
			t.Fatalf("missing field digest %q in %#v", field, ref.Fields)
		}
		if field != "comment" && fd.Before == fd.After {
			t.Fatalf("field %q before==after digest (%s); expected change", field, fd.After)
		}
	}
	if ref.Fields["title"].Before != digestString("Fix API") || ref.Fields["title"].After != digestString("Fix API v2") {
		t.Fatalf("title digests wrong: %#v", ref.Fields["title"])
	}
}

func TestGitHubUpdateWorkItemAssignee(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		want      string
	}{
		{name: "set", requested: "octocat", want: "octocat"},
		{name: "clear", requested: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newIssueMock()
			m.assignees = []string{"mona"}
			rec := &recordingRecorder{}
			p, repo := newIssueProvider(t, m, WithMutationRecorder(rec))
			assignee := tc.requested

			item, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
				Repository: repo,
				ID:         "7",
				Assignee:   &assignee,
			})
			if err != nil {
				t.Fatalf("UpdateWorkItem: %v", err)
			}
			if item.Assignee != tc.want {
				t.Fatalf("assignee = %q, want %q", item.Assignee, tc.want)
			}
			values, ok := m.patchBody["assignees"].([]interface{})
			if !ok {
				t.Fatalf("PATCH assignees = %#v, want an array", m.patchBody["assignees"])
			}
			if tc.want == "" {
				if len(values) != 0 {
					t.Fatalf("PATCH assignees = %#v, want []", values)
				}
			} else if len(values) != 1 || values[0] != tc.want {
				t.Fatalf("PATCH assignees = %#v, want [%q]", values, tc.want)
			}
			ref, ok := rec.last()
			if !ok {
				t.Fatal("expected an external-ref mutation to be recorded")
			}
			wantDigest := FieldDigest{Before: digestString("mona"), After: digestString(tc.want)}
			if got := ref.Fields["assignee"]; got != wantDigest {
				t.Fatalf("assignee digest = %#v, want %#v", got, wantDigest)
			}
		})
	}
}

func TestGitHubUpdateWorkItemNoChangeSkipsRecord(t *testing.T) {
	m := newIssueMock()
	m.assignees = []string{"mona"}
	rec := &recordingRecorder{}
	p, repo := newIssueProvider(t, m, WithMutationRecorder(rec))
	item, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{Repository: repo, ID: "7"})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if item.Assignee != "mona" {
		t.Fatalf("no-op update assignee = %q, want mona", item.Assignee)
	}
	if _, ok := rec.last(); ok {
		t.Fatal("no-op update should record no mutation")
	}
	if m.patchBody != nil {
		t.Fatalf("no-op update should not PATCH, got %#v", m.patchBody)
	}
}

func TestGitHubUpdateWorkItemAssignsExistingMilestoneAndRecordsDigest(t *testing.T) {
	m := newIssueMock()
	m.milestone = 3
	rec := &recordingRecorder{}
	p, repo := newIssueProvider(t, m, WithMutationRecorder(rec))
	milestone := 8

	item, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository: repo,
		ID:         "7",
		Milestone:  &milestone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if item.Parent == nil || item.Parent.Type != "milestone" || item.Parent.ID != "8" {
		t.Fatalf("final milestone = %#v, want milestone 8", item.Parent)
	}
	if got := m.patchBody["milestone"]; got != float64(8) {
		t.Fatalf("PATCH milestone = %#v, want 8", got)
	}
	ref, ok := rec.last()
	if !ok {
		t.Fatal("expected milestone mutation to be recorded")
	}
	if ref.Operation != "milestone" {
		t.Fatalf("operation = %q, want milestone", ref.Operation)
	}
	want := FieldDigest{Before: digestString("3"), After: digestString("8")}
	if got := ref.Fields["milestone"]; got != want {
		t.Fatalf("milestone digest = %#v, want %#v", got, want)
	}
}

func TestGitHubUpdateWorkItemRejectsInvalidMilestone(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	milestone := 0

	if _, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository: repo,
		ID:         "7",
		Milestone:  &milestone,
	}); err == nil {
		t.Fatal("UpdateWorkItem with milestone 0: err = nil, want an error")
	}
	if m.patchBody != nil {
		t.Fatalf("invalid milestone should not PATCH, got %#v", m.patchBody)
	}
}

// TestGitHubUpdateCommentEditsInPlace is #716's sticky-comment primitive: a
// caller with an existing comment's ID edits its body via PATCH rather than
// posting a new one — GitHub's comment-edit endpoint, not previously wired.
func TestGitHubUpdateCommentEditsInPlace(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)

	if err := p.postComment(context.Background(), repo, "7", "original body"); err != nil {
		t.Fatalf("postComment: %v", err)
	}
	m.mu.Lock()
	commentID := fmt.Sprint(m.comments[0]["id"])
	m.mu.Unlock()

	if err := p.UpdateComment(context.Background(), repo, commentID, "edited body"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.comments) != 1 {
		t.Fatalf("comments = %v, want exactly 1 — an edit must not create a second comment", m.comments)
	}
	if m.comments[0]["body"] != "edited body" {
		t.Fatalf("comment body = %v, want %q", m.comments[0]["body"], "edited body")
	}
}

func TestGitHubUpdateCommentRequiresID(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	if err := p.UpdateComment(context.Background(), repo, "", "body"); err == nil {
		t.Fatal("UpdateComment with an empty comment id: err = nil, want an error")
	}
}

func TestGitHubDeleteCommentIsIdempotent(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	if err := p.postComment(context.Background(), repo, "7", "obsolete"); err != nil {
		t.Fatalf("postComment: %v", err)
	}
	m.mu.Lock()
	commentID := fmt.Sprint(m.comments[0]["id"])
	m.mu.Unlock()

	if err := p.DeleteComment(context.Background(), repo, commentID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if err := p.DeleteComment(context.Background(), repo, commentID); err != nil {
		t.Fatalf("DeleteComment retry: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.comments) != 0 {
		t.Fatalf("comments = %v, want none", m.comments)
	}
}

func TestGitHubDeleteCommentRequiresID(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	if err := p.DeleteComment(context.Background(), repo, ""); err == nil {
		t.Fatal("DeleteComment with an empty comment id: err = nil, want an error")
	}
}

func TestGitHubClaimSingleWinnerUnderConcurrency(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)

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
	// Both runs must agree on who won.
	if results[0].ClaimedBy != results[1].ClaimedBy {
		t.Fatalf("runs disagree on winner: %q vs %q", results[0].ClaimedBy, results[1].ClaimedBy)
	}
	if winner.ClaimedBy != "run-A" && winner.ClaimedBy != "run-B" {
		t.Fatalf("winner not one of the racers: %q", winner.ClaimedBy)
	}
	// The winner's item carries the claimed label for human visibility. (A loser's
	// snapshot may predate the winner's label write, so we assert on the winner.)
	if !winner.Item.HasLabel(LabelClaimed) {
		t.Fatalf("claimed label not applied to winner: %#v", winner.Item.Labels)
	}
}

func TestGitHubClaimIdempotentAndAlreadyClaimed(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()

	first, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	// Re-claim by the same run is idempotent and must not post another breadcrumb.
	before := len(m.comments)
	again, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !again.Claimed || again.ClaimedBy != "run-A" {
		t.Fatalf("re-claim = %+v, %v", again, err)
	}
	if len(m.comments) != before {
		t.Fatalf("idempotent re-claim posted extra comment: %d -> %d", before, len(m.comments))
	}
	// A different run loses and does not post a breadcrumb (fast path).
	other, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-B"})
	if err != nil {
		t.Fatalf("loser claim error: %v", err)
	}
	if other.Claimed || other.ClaimedBy != "run-A" {
		t.Fatalf("expected run-B to lose to run-A, got %+v", other)
	}
	if len(m.comments) != before {
		t.Fatalf("losing claim should not post a breadcrumb: %d -> %d", before, len(m.comments))
	}
}

func TestGitHubClaimCanBeReacquiredAfterRelease(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()

	first, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	// Preserve a losing racer breadcrumb from the old epoch. Releasing run-A
	// must retire this too rather than promote it to the next winner.
	if err := p.postComment(ctx, repo, "7", claimBreadcrumb("run-racer")); err != nil {
		t.Fatalf("post losing racer breadcrumb: %v", err)
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

	next, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-B"})
	if err != nil {
		t.Fatalf("follow-up claim: %v", err)
	}
	if !next.Claimed || next.ClaimedBy != "run-B" {
		t.Fatalf("follow-up claim = %+v, want run-B to own the new epoch", next)
	}
	if !next.Item.HasLabel(LabelClaimed) {
		t.Fatalf("follow-up item labels = %v, want %q restored", next.Item.Labels, LabelClaimed)
	}
}

func TestGitHubClaimIgnoresForgedRelease(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()

	first, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	m.mu.Lock()
	m.nextID++
	m.comments = append(m.comments, map[string]interface{}{
		"id":   m.nextID,
		"body": claimReleaseBreadcrumb("run-A"),
		"user": map[string]string{"login": "mallory"},
	})
	m.mu.Unlock()

	other, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-B"})
	if err != nil {
		t.Fatalf("claim after forged release: %v", err)
	}
	if other.Claimed || other.ClaimedBy != "run-A" {
		t.Fatalf("claim after forged release = %+v, want run-A to remain owner", other)
	}
}

func TestGitHubLedgerAuthorizedReleaseReconcilesHistoricalWinner(t *testing.T) {
	m := newIssueMock()
	m.labels = append(m.labels, LabelClaimed)
	m.comments = append(m.comments, map[string]interface{}{
		"id":   int64(1),
		"body": claimBreadcrumb("historical-run"),
		"user": map[string]string{"login": "goobers"},
	})
	m.nextID = 1
	p, repo := newIssueProvider(t, m)

	released, err := p.ReleaseWorkItemClaim(context.Background(), ClaimWorkItemRequest{
		Repository:       repo,
		ID:               "7",
		RunID:            "current-run",
		LedgerAuthorized: true,
	})
	if err != nil {
		t.Fatalf("ledger-authorized release: %v", err)
	}
	if released.HasLabel(LabelClaimed) {
		t.Fatalf("released item still has %q: %v", LabelClaimed, released.Labels)
	}
	winner, claimed, err := p.claimWinner(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("claimWinner after reconciliation: %v", err)
	}
	if claimed {
		t.Fatalf("claimWinner after reconciliation = %q, want no active claim", winner)
	}
}

func TestGitHubReconcileOrphanedClaimClosesEpochAndExplainsLabels(t *testing.T) {
	m := newIssueMock()
	m.labels = append(m.labels, LabelClaimed, LabelReady)
	m.comments = append(m.comments, map[string]interface{}{
		"id":   int64(1),
		"body": claimBreadcrumb("historical-run"),
		"user": map[string]string{"login": "goobers"},
	})
	m.nextID = 1
	p, repo := newIssueProvider(t, m)

	item, err := p.ReconcileOrphanedWorkItemClaim(
		context.Background(),
		repo,
		"7",
		[]string{LabelClaimed, LabelReady},
		"Removed drifted claim and ready labels.",
	)
	if err != nil {
		t.Fatalf("ReconcileOrphanedWorkItemClaim: %v", err)
	}
	if item.HasLabel(LabelClaimed) || item.HasLabel(LabelReady) {
		t.Fatalf("reconciled labels = %v, want claim and ready removed", item.Labels)
	}
	winner, claimed, err := p.claimWinner(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("claimWinner: %v", err)
	}
	if claimed {
		t.Fatalf("claimWinner = %q, want closed epoch", winner)
	}
	last := m.comments[len(m.comments)-1]["body"].(string)
	if !strings.Contains(last, "Removed drifted claim and ready labels.") {
		t.Fatalf("last comment = %q, want explanation", last)
	}
}

func TestGitHubRateLimitBackoffAndTelemetry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	reset := time.Now().Add(2 * time.Second).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset))
			http.Error(w, "secondary rate limit", http.StatusForbidden)
			return
		}
		writeJSON(t, w, map[string]interface{}{"id": 123, "number": 7, "title": "ok", "state": "open"})
	}))
	defer srv.Close()

	obs := &recordingObserver{}
	var waits []time.Duration
	p := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.BaseURL = srv.URL
	}, WithRateLimitObserver(obs))
	p.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	item, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err != nil {
		t.Fatalf("GetWorkItem under rate limit: %v", err)
	}
	if item.Title != "ok" {
		t.Fatalf("expected success after backoff, got %#v", item)
	}
	if obs.count() != 2 {
		t.Fatalf("expected 2 rate-limit telemetry events, got %d", obs.count())
	}
	if len(waits) != 2 {
		t.Fatalf("expected 2 backoff sleeps, got %v", waits)
	}
	for _, wt := range waits {
		if wt <= 0 {
			t.Fatalf("backoff wait not honored: %v", waits)
		}
	}
	if !obs.events[0].Secondary || obs.events[0].RetryAfter != time.Second {
		t.Fatalf("expected secondary rate-limit event honoring Retry-After, got %#v", obs.events[0])
	}
	if obs.events[0].Provider != ProviderGitHub ||
		obs.events[0].Scope == "" ||
		obs.events[0].Delay != waits[0] ||
		obs.events[0].Outcome != RateLimitOutcomeRetry {
		t.Fatalf("incomplete rate-limit telemetry event: %#v", obs.events[0])
	}
}

func TestGitHubRateLimitGivesUpAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	obs := &recordingObserver{}
	p := NewGitHubProvider("token",
		func(p *GitHubProvider) { p.BaseURL = srv.URL },
		WithMaxRateLimitRetries(2),
		WithRateLimitObserver(obs),
	)
	p.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err == nil {
		t.Fatal("expected error after exhausting rate-limit retries")
	}
	// The give-up error is typed (#614), never the generic non-2xx string.
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want *RateLimitError", err, err)
	}
	if !rl.Secondary {
		t.Fatalf("Retry-After-driven limit should mark Secondary, got %+v", rl)
	}
	if obs.count() != 3 || obs.events[2].Outcome != RateLimitOutcomeExhausted {
		t.Fatalf("rate-limit outcomes = %#v, want two retries and exhausted", obs.events)
	}
}

func TestGitHubForbiddenResponsePreservesRetryGuidance(t *testing.T) {
	cases := []struct {
		name          string
		headers       map[string]string
		wantTransient bool
		wantCalls     int
		wantGuidance  string
	}{
		{
			name:          "secondary rate limit",
			headers:       map[string]string{"Retry-After": "1"},
			wantTransient: true,
			wantCalls:     2,
			wantGuidance:  `Retry-After="1"`,
		},
		{
			name: "primary rate limit",
			headers: map[string]string{
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Reset":     "1784210000",
			},
			wantTransient: true,
			wantCalls:     2,
			wantGuidance:  `X-RateLimit-Reset="1784210000"`,
		},
		{
			name:          "authorization",
			wantTransient: false,
			wantCalls:     1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				for key, value := range tc.headers {
					w.Header().Set(key, value)
				}
				http.Error(w, "forbidden", http.StatusForbidden)
			}))
			defer srv.Close()

			p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL }, WithMaxRateLimitRetries(1))
			p.sleep = func(context.Context, time.Duration) error { return nil }
			_, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
			if err == nil {
				t.Fatal("expected forbidden response to fail")
			}
			if got := IsTransientError(err); got != tc.wantTransient {
				t.Fatalf("IsTransientError(%v) = %v, want %v", err, got, tc.wantTransient)
			}
			if calls != tc.wantCalls {
				t.Fatalf("provider calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantGuidance != "" && !strings.Contains(err.Error(), tc.wantGuidance) {
				t.Fatalf("error %q does not preserve %q", err, tc.wantGuidance)
			}
		})
	}
}

func TestGitHubListWorkItemsFiltersAndPagination(t *testing.T) {
	var gotQuery map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery = map[string]string{
			"assignee": q.Get("assignee"), "since": q.Get("since"),
			"page": q.Get("page"), "per_page": q.Get("per_page"), "labels": q.Get("labels"),
		}
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 7, "title": "issue", "state": "open"},
			{"id": 2, "number": 8, "title": "a pr", "state": "open", "pull_request": map[string]string{"url": "pr-url"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL })
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	items, err := p.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Labels:     []string{LabelReady}, Assignee: "mona", UpdatedSince: &since, Limit: 50, Page: 2,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	// The pull request entry must be excluded from a backlog issues query.
	if len(items) != 1 || items[0].ID != "7" {
		t.Fatalf("expected only the issue (PR excluded), got %#v", items)
	}
	// per_page stays the requested Limit (50): plain Labels is reliably
	// server-filtered via GitHub's own `labels` query param (#2067's
	// oversized-scan trigger is LabelPredicate/FieldPredicate only — Labels
	// alone never needs it, on either provider's shared check).
	if gotQuery["assignee"] != "mona" || gotQuery["since"] != "2026-07-01T00:00:00Z" ||
		gotQuery["page"] != "2" || gotQuery["per_page"] != "50" || gotQuery["labels"] != LabelReady {
		t.Fatalf("query params not wired: %#v", gotQuery)
	}
}

func TestGitHubListWorkItemsLimitSkipsPRsAcrossPages(t *testing.T) {
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			writeJSON(t, w, []map[string]interface{}{{
				"id": 1, "number": 7, "title": "pull request", "state": "open",
				"pull_request": map[string]string{"url": "pr-url"},
			}})
			return
		}
		writeJSON(t, w, []map[string]interface{}{{
			"id": 2, "number": 8, "title": "issue", "state": "open",
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	provider := NewGitHubProvider("token", func(provider *GitHubProvider) { provider.BaseURL = srv.URL })

	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "8" || requests != 2 {
		t.Fatalf("items = %#v, requests = %d; want issue 8 after PR-only first page", items, requests)
	}
}

// TestGitHubListWorkItemsOversizedScanFindsMatchBeyondTruncationBoundary is
// #2067's regression test for GitHub's caller-paged path: with Limit=1 and
// a LabelPredicate, the first two raw candidates fail the predicate (one is
// a pull request, one carries the wrong label) but a third matches. Before
// the fix, per_page was pinned to min(req.Limit, 100) = 1, so the single
// fetched candidate was the pull request and ListWorkItems returned zero
// items despite a real match existing two candidates later — silently
// breaking "Limit = up to N matches". The fix gives the fetch GitHub's own
// 100-per-page ceiling whenever a post-fetch filter (here, LabelPredicate)
// is active, so the match is found in one request.
func TestGitHubListWorkItemsOversizedScanFindsMatchBeyondTruncationBoundary(t *testing.T) {
	predicate, err := labelpredicate.Compile(`"wanted" in labels`, nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	requests := 0
	var gotPerPage string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotPerPage = r.URL.Query().Get("per_page")
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 1, "title": "pr 1", "state": "open", "pull_request": map[string]string{"url": "pr-1"}},
			{"id": 2, "number": 2, "title": "other issue", "state": "open", "labels": []map[string]string{{"name": "other"}}},
			{"id": 3, "number": 3, "title": "wanted issue", "state": "open", "labels": []map[string]string{{"name": "wanted"}}},
		})
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(provider *GitHubProvider) { provider.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}

	pageInfo := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: repo, LabelPredicate: predicate, Limit: 1, PageInfo: pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "3" || requests != 1 {
		t.Fatalf("items = %#v, requests = %d; want issue 3 found in a single request", items, requests)
	}
	if gotPerPage != "100" {
		t.Fatalf("per_page = %q, want 100 (oversized: LabelPredicate is active)", gotPerPage)
	}
	if pageInfo.HasNext {
		t.Fatalf("page info = %+v, want HasNext=false (every fetched candidate was scanned, and the fetch wasn't itself capped)", pageInfo)
	}
}

// TestGitHubListWorkItemsPageInfoHasNextWhenFetchItselfCapped proves the
// OTHER half of #2067's HasNext contract: even when every fetched candidate
// was scanned without finding Limit matches, HasNext must still be true if
// the fetch itself hit the per-page ceiling — GitHub may hold further
// candidates beyond what this round asked for.
func TestGitHubListWorkItemsPageInfoHasNextWhenFetchItselfCapped(t *testing.T) {
	predicate, err := labelpredicate.Compile(`"wanted" in labels`, nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const pageSize = 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issues := make([]map[string]interface{}, pageSize)
		for i := range issues {
			issues[i] = map[string]interface{}{
				"id": i + 1, "number": i + 1, "title": "issue", "state": "open",
				"labels": []map[string]string{{"name": "other"}},
			}
		}
		writeJSON(t, w, issues)
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(provider *GitHubProvider) { provider.BaseURL = server.URL })

	pageInfo := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, LabelPredicate: predicate, Limit: 1, PageInfo: pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want none (no candidate carries the wanted label)", items)
	}
	if !pageInfo.HasNext || pageInfo.NextCursor != strconv.Itoa(pageSize) {
		t.Fatalf("page info = %+v, want HasNext=true NextCursor=%d (the fetch itself hit the per-page ceiling)", pageInfo, pageSize)
	}
}

func TestGitHubListWorkItemsProjectsAndFiltersNativeFields(t *testing.T) {
	predicate, err := fieldpredicate.Compile(`fields["number"] >= 2 && fields["locked"] == false`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"id": 101, "number": 1, "title": "first", "state": "open"},
			{
				"id": 102, "number": 2, "title": "second", "state": "open",
				"milestone": map[string]interface{}{"number": 7, "title": "V1"},
			},
		})
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(provider *GitHubProvider) { provider.BaseURL = server.URL })

	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:     RepositoryRef{Owner: "acme", Name: "app"},
		FieldPredicate: predicate,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "2" {
		t.Fatalf("items = %#v, want issue 2", items)
	}
	if got := items[0].Fields["milestone.title"]; got != "V1" {
		t.Fatalf("milestone.title = %#v, want V1", got)
	}
	if got := items[0].Fields["milestone.number"]; got != int64(7) {
		t.Fatalf("milestone.number = %#v, want int64(7)", got)
	}
}

func TestGitHubListWorkItemsUnavailableNativeFieldFails(t *testing.T) {
	predicate, err := fieldpredicate.Compile(`fields["project.priority"] == 1`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]interface{}{{"id": 101, "number": 1, "title": "first", "state": "open"}})
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(provider *GitHubProvider) { provider.BaseURL = server.URL })

	_, err = provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:     RepositoryRef{Owner: "acme", Name: "app"},
		FieldPredicate: predicate,
	})
	if err == nil || !strings.Contains(err.Error(), `field "project.priority" is unavailable`) {
		t.Fatalf("ListWorkItems error = %v, want unavailable-field error", err)
	}
}

func TestGitHubTokenSourceResolvesPerRequest(t *testing.T) {
	m := newIssueMock()
	ts := &staticTokenSource{token: "dynamic-token"}
	p, repo := newIssueProvider(t, m, WithTokenSource(ts))
	if _, err := p.GetWorkItem(context.Background(), repo, "7"); err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if ts.calls == 0 {
		t.Fatal("expected token source to be consulted")
	}
	if m.authSeen != "Bearer dynamic-token" {
		t.Fatalf("expected token-source token in Authorization header, got %q", m.authSeen)
	}
}
