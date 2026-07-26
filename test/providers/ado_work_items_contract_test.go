package providerscontract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type adoWorkItemBackend struct {
	mu       sync.Mutex
	revision int
	title    string
	body     string
	state    string
	tags     []string
	assignee string
	comments []map[string]interface{}
	query    string

	completedState    string
	commentStatus     int
	claimBarrier      chan struct{}
	claimReads        int
	revisionConflicts int
}

func newADOWorkItemBackend() *adoWorkItemBackend {
	return &adoWorkItemBackend{
		revision:       3,
		title:          "Fix API",
		body:           "do it",
		state:          "Active",
		tags:           []string{"route/backend"},
		assignee:       "Mona",
		completedState: "Done",
	}
}

func (b *adoWorkItemBackend) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode WIQL request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.query = body["query"]
		b.mu.Unlock()
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 42}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/$Issue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("create method = %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		var patch []adoContractPatch
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Errorf("decode create patch: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		created := &adoWorkItemBackend{revision: 1, state: "New"}
		created.apply(patch)
		writeJSON(t, w, created.item(43))
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			b.mu.Lock()
			item := b.item(42)
			barrier := b.claimBarrier
			if barrier != nil {
				b.claimReads++
				if b.claimReads == 2 {
					close(barrier)
				}
			}
			b.mu.Unlock()
			if barrier != nil {
				<-barrier
			}
			writeJSON(t, w, item)
		case http.MethodPatch:
			b.mu.Lock()
			defer b.mu.Unlock()
			var patch []adoContractPatch
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode update patch: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(patch) == 0 || patch[0].Op != "test" || patch[0].Path != "/rev" {
				t.Errorf("update omitted revision test: %#v", patch)
				http.Error(w, "missing revision", http.StatusBadRequest)
				return
			}
			if revision, ok := numberAsInt(patch[0].Value); !ok || revision != b.revision {
				b.revisionConflicts++
				http.Error(w, "revision does not match", http.StatusPreconditionFailed)
				return
			}
			b.apply(patch[1:])
			b.revision++
			writeJSON(t, w, b.item(42))
		default:
			t.Errorf("work-item method = %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"comments": b.comments})
		case http.MethodPost:
			if b.commentStatus != 0 {
				http.Error(w, "comment failed", b.commentStatus)
				return
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode comment: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			comment := map[string]interface{}{
				"id": len(b.comments) + 1, "text": body["text"],
				"createdBy":   map[string]string{"displayName": "Goobers Bot"},
				"createdDate": "2026-07-26T12:00:00Z",
				"url":         "comment-url",
			}
			b.comments = append(b.comments, comment)
			writeJSON(t, w, comment)
		default:
			t.Errorf("comments method = %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		completedState := b.completedState
		b.mu.Unlock()
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": completedState, "category": "Completed"},
		}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

type adoContractPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func (b *adoWorkItemBackend) item(id int) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "rev": b.revision, "url": fmt.Sprintf("https://dev.azure.com/org/project/_workitems/edit/%d", id),
		"fields": map[string]interface{}{
			"System.WorkItemType": "Issue",
			"System.Title":        b.title,
			"System.Description":  b.body,
			"System.State":        b.state,
			"System.Tags":         strings.Join(b.tags, "; "),
			"System.AssignedTo":   map[string]string{"displayName": b.assignee},
			"System.CreatedDate":  "2026-07-01T12:00:00Z",
			"System.ChangedDate":  "2026-07-26T12:00:00Z",
		},
	}
}

func (b *adoWorkItemBackend) apply(patch []adoContractPatch) {
	for _, operation := range patch {
		value, _ := operation.Value.(string)
		switch operation.Path {
		case "/fields/System.Title":
			b.title = value
		case "/fields/System.Description":
			b.body = value
		case "/fields/System.State":
			b.state = value
		case "/fields/System.Tags":
			b.tags = splitADOTags(value)
		case "/fields/System.AssignedTo":
			b.assignee = value
		}
	}
}

func splitADOTags(value string) []string {
	var tags []string
	for _, tag := range strings.Split(value, ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func numberAsInt(value interface{}) (int, bool) {
	number, ok := value.(float64)
	return int(number), ok
}

func TestContract_ADOWorkItemOperations(t *testing.T) {
	backend := newADOWorkItemBackend()
	server := backend.server(t)
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"}
	ctx := context.Background()
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:   repo,
		Labels:       []string{"route/backend"},
		State:        "open",
		Assignee:     "Mona",
		UpdatedSince: &since,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("ListWorkItems = %#v, %v", items, err)
	}
	if items[0].State != "open" || items[0].CreatedAt == nil || items[0].UpdatedAt == nil {
		t.Fatalf("mapped item = %#v", items[0])
	}
	backend.mu.Lock()
	query := backend.query
	backend.mu.Unlock()
	for _, clause := range []string{"[System.AssignedTo] = 'Mona'", "[System.Tags] CONTAINS 'route/backend'", "[System.ChangedDate] >="} {
		if !strings.Contains(query, clause) {
			t.Errorf("WIQL query %q does not contain %q", query, clause)
		}
	}
	if strings.Contains(query, "[System.State]") {
		t.Errorf("common state should be category-filtered after item reads, got WIQL %q", query)
	}

	created, err := provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
		Repository: repo,
		Title:      "New work",
		Body:       "details",
		Labels:     []string{"goobers:ready"},
		Assignee:   "Mona",
		RunID:      "run-create",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	if created.ID != "43" || created.Title != "New work" || !created.HasLabel("goobers:ready") ||
		!strings.Contains(created.Body, "goobers run-id: run-create") {
		t.Fatalf("created item = %#v", created)
	}

	title := "Updated work"
	body := "updated details"
	updated, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repo,
		ID:           "42",
		Title:        &title,
		Body:         &body,
		AddLabels:    []string{"goobers:ready"},
		RemoveLabels: []string{"route/backend"},
		Comment:      "updated by contract",
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if updated.Title != title || updated.Body != body || !updated.HasLabel("goobers:ready") || updated.HasLabel("route/backend") {
		t.Fatalf("updated item = %#v", updated)
	}

	status, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
		Repository: repo, ID: "42", Status: providers.WorkItemStatusInProgress,
	})
	if err != nil || status.Status != providers.WorkItemStatusInProgress {
		t.Fatalf("UpdateWorkItemStatus = %#v, %v", status, err)
	}

	closed, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: "42", State: "closed",
	})
	if err != nil || closed.State != "closed" {
		t.Fatalf("close UpdateWorkItem = %#v, %v", closed, err)
	}
	reopened, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: "42", State: "open",
	})
	if err != nil || reopened.State != "open" {
		t.Fatalf("reopen UpdateWorkItem = %#v, %v", reopened, err)
	}
	done, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
		Repository: repo, ID: "42", Status: providers.WorkItemStatusDone,
	})
	if err != nil || done.State != "closed" || done.Status != providers.WorkItemStatusDone {
		t.Fatalf("done UpdateWorkItemStatus = %#v, %v", done, err)
	}

	comments, err := provider.ListComments(ctx, repo, "42")
	if err != nil || len(comments) != 1 || comments[0].Body != "updated by contract" || comments[0].Author != "Goobers Bot" {
		t.Fatalf("ListComments = %#v, %v", comments, err)
	}
}

func TestContract_ADOCustomStateCategoryMapping(t *testing.T) {
	backend := newADOWorkItemBackend()
	backend.state = "Finished"
	backend.completedState = "Finished"
	server := backend.server(t)
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project"}

	item, err := provider.GetWorkItem(context.Background(), repo, "42")
	if err != nil || item.State != "closed" {
		t.Fatalf("GetWorkItem = %#v, %v", item, err)
	}
	closed, err := provider.ListWorkItems(context.Background(), providers.ListWorkItemsRequest{
		Repository: repo,
		State:      "closed",
	})
	if err != nil || len(closed) != 1 {
		t.Fatalf("closed ListWorkItems = %#v, %v", closed, err)
	}
	open, err := provider.ListWorkItems(context.Background(), providers.ListWorkItemsRequest{
		Repository: repo,
		State:      "open",
	})
	if err != nil || len(open) != 0 {
		t.Fatalf("open ListWorkItems = %#v, %v", open, err)
	}
}

func TestContract_ADOUpdateReportsCommittedFieldsWhenCommentFails(t *testing.T) {
	backend := newADOWorkItemBackend()
	backend.commentStatus = http.StatusInternalServerError
	server := backend.server(t)
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	title := "Committed title"
	item, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project"},
		ID:         "42",
		Title:      &title,
		Comment:    "fails",
	})
	if err == nil || !strings.Contains(err.Error(), "update committed") {
		t.Fatalf("UpdateWorkItem error = %v", err)
	}
	if item.Title != title {
		t.Fatalf("committed item = %#v", item)
	}
}

func TestContract_ADOListCommentsPaginates(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if token := r.URL.Query().Get("continuationToken"); token == "" {
			w.Header().Set("x-ms-continuationtoken", "page-2")
			writeJSON(t, w, map[string]interface{}{"comments": []map[string]interface{}{{
				"id": 1, "text": "first", "createdDate": "2026-07-26T12:00:00Z",
			}}})
			return
		} else if token != "page-2" {
			t.Errorf("continuationToken = %q", token)
			http.Error(w, "bad continuation token", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, map[string]interface{}{"comments": []map[string]interface{}{{
			"id": 2, "text": "second", "createdDate": "2026-07-26T12:01:00Z",
		}}})
	}))
	defer server.Close()
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})

	comments, err := provider.ListComments(
		context.Background(),
		providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project"},
		"42",
	)
	if err != nil || len(comments) != 2 || comments[0].Body != "first" || comments[1].Body != "second" {
		t.Fatalf("ListComments = %#v, %v", comments, err)
	}
	if requests != 2 {
		t.Fatalf("comment requests = %d, want 2", requests)
	}
}

func TestContract_ADOClaimExactlyOneWinner(t *testing.T) {
	backend := newADOWorkItemBackend()
	backend.claimBarrier = make(chan struct{})
	server := backend.server(t)
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) {
		p.BaseURL = server.URL
	})
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project", Name: "repo"}

	var wg sync.WaitGroup
	results := make([]providers.ClaimResult, 2)
	errs := make([]error, 2)
	runIDs := []string{"run-A", "run-B"}
	wg.Add(len(runIDs))
	for i := range runIDs {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = provider.ClaimWorkItem(context.Background(), providers.ClaimWorkItemRequest{
				Repository: repo,
				ID:         "42",
				RunID:      runIDs[i],
			})
		}(i)
	}
	wg.Wait()

	winners := 0
	for i, result := range results {
		if errs[i] != nil {
			t.Fatalf("claim %s: %v", runIDs[i], errs[i])
		}
		if result.Claimed {
			winners++
		}
		if result.ClaimedBy == "" || !result.Item.HasLabel(providers.LabelClaimed) {
			t.Fatalf("claim result = %#v", result)
		}
		for _, label := range result.Item.Labels {
			if strings.HasPrefix(label, "goobers:claim-run:") {
				t.Fatalf("internal owner tag leaked through labels: %#v", result.Item.Labels)
			}
		}
	}
	if winners != 1 || results[0].ClaimedBy != results[1].ClaimedBy {
		t.Fatalf("claims did not settle on one winner: %#v", results)
	}
	backend.mu.Lock()
	revisionConflicts := backend.revisionConflicts
	backend.mu.Unlock()
	if revisionConflicts != 1 {
		t.Fatalf("revision conflicts = %d, want 1", revisionConflicts)
	}

	released, err := provider.ReleaseWorkItemClaim(context.Background(), providers.ClaimWorkItemRequest{
		Repository: repo,
		ID:         "42",
		RunID:      results[0].ClaimedBy,
	})
	if err != nil {
		t.Fatalf("ReleaseWorkItemClaim: %v", err)
	}
	if released.HasLabel(providers.LabelClaimed) {
		t.Fatalf("claim label remained after release: %#v", released.Labels)
	}
}

func TestContract_ADOWorkItemErrorMapping(t *testing.T) {
	tests := []struct {
		status        int
		wantNotFound  bool
		wantTransient bool
	}{
		{http.StatusBadRequest, false, false},
		{http.StatusUnauthorized, false, false},
		{http.StatusForbidden, false, false},
		{http.StatusNotFound, true, false},
		{http.StatusConflict, false, false},
		{http.StatusPreconditionFailed, false, false},
		{http.StatusTooManyRequests, false, true},
		{http.StatusInternalServerError, false, true},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(test.status), test.status)
			}))
			defer server.Close()
			provider := providers.NewADOProvider(
				"org",
				"project",
				"token",
				func(p *providers.ADOProvider) { p.BaseURL = server.URL },
				providers.WithADOMaxRateLimitRetries(0),
			)
			_, err := provider.GetWorkItem(
				context.Background(),
				providers.RepositoryRef{Provider: providers.ProviderADO, Project: "project"},
				"42",
			)
			if err == nil {
				t.Fatal("expected provider error")
			}
			if got := providers.IsNotFoundError(err); got != test.wantNotFound {
				t.Errorf("IsNotFoundError = %t, want %t", got, test.wantNotFound)
			}
			if got := providers.IsTransientError(err); got != test.wantTransient {
				t.Errorf("IsTransientError = %t, want %t", got, test.wantTransient)
			}
		})
	}
}
