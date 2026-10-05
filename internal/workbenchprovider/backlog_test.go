package workbenchprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1, Body: io.NopCloser(strings.NewReader(body))}
}

func source(provider apiv1.Provider) (workbench.Scope, workbench.BoundSource) {
	target := apiv1.InteractiveRepositoryIdentity{Provider: provider, Owner: "org", Name: "repo"}
	project := "org/repo"
	if provider == apiv1.ProviderADO {
		target.Name = ""
		target.Project = "project"
		project = "project"
	}
	return workbench.Scope{GaggleID: "g", Bindings: map[string]bool{"backlog": true}}, workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "backlog", Kind: "backlog", Objectives: &apiv1.WorkbenchObjectiveSelector{IDs: []string{"101"}, Types: []string{"Epic"}}}, Backlog: apiv1.BacklogRef{Provider: provider, Project: project}, BacklogIdentity: target}
}

func TestGitHubNativePageAndLocatorIdentity(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Host != "api.github.com" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		switch r.URL.Path {
		case "/repos/org/repo/issues":
			if r.URL.Query().Get("per_page") != "2" || r.URL.Query().Get("sort") != "created" || r.URL.Query().Get("labels") != "" {
				t.Fatalf("unbounded/filtered page: %s", r.URL)
			}
			if r.URL.Query().Get("page") == "2" {
				return response(`[]`), nil
			}
			return response(`[{"id":101,"number":7,"title":"First","body":"source","html_url":"https://github.com/org/repo/issues/7","state":"open","updated_at":"2026-10-01T01:00:00Z","assignees":[{"login":"a"},{"login":"b"}],"milestone":{"id":501,"number":2,"title":"v1"}},{"id":102,"number":8,"pull_request":{"url":"ignored"}}]`), nil
		case "/repos/org/repo/issues/7":
			return response(`{"id":101,"number":7,"title":"Renamed","html_url":"https://github.com/org/repo/issues/7","state":"open"}`), nil
		default:
			t.Fatalf("unexpected relation expansion: %s", r.URL)
			return nil, errors.New("unexpected request")
		}
	})}
	scope, bound := source(apiv1.ProviderGitHub)
	r, err := NewBacklogReader(scope, bound, providers.NewGitHubProvider("", providers.WithHTTPClient(client)))
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.Page(context.Background(), workbench.BacklogPageRequest{Limit: 2})
	if err != nil || len(page.Items) != 1 || calls != 1 || !page.Partial || page.Exhausted || page.Omitted != 1 || page.NextCursor == "" {
		t.Fatalf("page: %+v %v calls=%d", page, err, calls)
	}
	item := page.Items[0]
	if item.Ref.SourceID != "101" || item.Locator.ID != "7" || !item.Objective || len(item.Assignees) != 2 || item.RevisionSemantics != "timestamp-preflight" {
		t.Fatalf("item: %+v", item)
	}
	if item.RelationshipCoverage.Parents != "not-loaded" || item.RelationshipCoverage.Blockers != "not-loaded" || len(item.Relationships) != 1 || item.Relationships[0].Kind != "milestone-member" || item.Relationships[0].Target.Ref.SourceID != "501" {
		t.Fatalf("relationship projection: %+v", item)
	}
	last, err := r.Page(context.Background(), workbench.BacklogPageRequest{Cursor: page.NextCursor})
	if err != nil || !last.Exhausted || last.Partial || calls != 2 {
		t.Fatalf("last page: %+v %v", last, err)
	}
	read, err := r.Get(context.Background(), workbench.BacklogItemRequest{ID: "7", ExpectedSourceID: "101"})
	if err != nil || read.Title != "Renamed" || read.Ref != item.Ref {
		t.Fatalf("rename changed identity: %+v %v", read, err)
	}
	_, err = r.Get(context.Background(), workbench.BacklogItemRequest{ID: "7", ExpectedSourceID: "999"})
	if !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("replacement accepted: %v", err)
	}
	otherBound := bound
	otherBound.Backlog.Project = "org/other"
	otherBound.BacklogIdentity.Name = "other"
	other, _ := NewBacklogReader(scope, otherBound, r.client)
	_, err = other.Page(context.Background(), workbench.BacklogPageRequest{Cursor: page.NextCursor})
	if !errors.Is(err, ErrInvalidCursor) || calls != 4 {
		t.Fatalf("foreign cursor read: %v calls=%d", err, calls)
	}
}

func TestADONativePageRevisionAndProjectFence(t *testing.T) {
	const native = `{"id":101,"rev":9,"url":"https://dev.azure.com/org/_apis/wit/workItems/101","fields":{"System.TeamProject":"project","System.WorkItemType":"Epic","System.State":"Active","System.Title":"Plan","System.Description":"description","Microsoft.VSTS.Common.AcceptanceCriteria":"criteria","System.Tags":"planning; goobers:claim-run:legacy","System.AssignedTo":{"displayName":"Ada","uniqueName":"ada@example.com"}},"relations":[{"rel":"System.LinkTypes.Hierarchy-Reverse","url":"https://dev.azure.com/org/_apis/wit/workItems/99"},{"rel":"System.LinkTypes.Dependency-Reverse","url":"https://dev.azure.com/org/other/_apis/wit/workItems/98"}]} `
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/wiql"):
			if r.Method != "POST" || r.URL.Query().Get("$top") != "2" {
				t.Fatalf("unbounded WIQL: %s %s", r.Method, r.URL)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body["query"], "[System.TeamProject] = @project") || !strings.Contains(body["query"], "ORDER BY [System.Id] ASC") {
				t.Fatalf("unscoped query %s", body["query"])
			}
			return response(`{"workItems":[{"id":101},{"id":102}]}`), nil
		case strings.HasSuffix(r.URL.Path, "/workitemsbatch"):
			if r.Method != "POST" {
				t.Fatal("batch must be read POST")
			}
			return response(`{"value":[` + native + `]}`), nil
		case strings.HasSuffix(r.URL.Path, "/states"):
			return response(`{"value":[{"name":"Active","category":"InProgress"}]}`), nil
		case strings.HasSuffix(r.URL.Path, "/workitems/101"):
			return response(strings.Replace(native, `"System.TeamProject":"project"`, `"System.TeamProject":"secret"`, 1)), nil
		default:
			t.Fatalf("unexpected fetch: %s", r.URL)
			return nil, errors.New("unexpected")
		}
	})}
	scope, bound := source(apiv1.ProviderADO)
	p := providers.NewADOProvider("org", "project", "", func(p *providers.ADOProvider) { p.Client = client })
	r, err := NewBacklogReader(scope, bound, p)
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.Page(context.Background(), workbench.BacklogPageRequest{Limit: 2})
	if err != nil || len(page.Items) != 1 || calls != 3 || page.Omitted != 1 || !page.Partial {
		t.Fatalf("ADO page %+v err=%v calls=%d", page, err, calls)
	}
	item := page.Items[0]
	if item.Ref.SourceID != "101" || item.Revision != "9" || item.State != "Active" || item.Description != "description" || item.AcceptanceCriteria != "criteria" || !item.Objective || !slices.Contains(item.Labels, "goobers:claim-run:legacy") || item.Assignees[0] != "ada@example.com" {
		t.Fatalf("ADO native data %+v", item)
	}
	if len(item.Relationships) != 2 || !item.Relationships[0].Incoming || item.Relationships[0].Target.Ref != nil || item.Relationships[1].Target.StableID != "98" || item.RelationshipCoverage.Milestones != "unsupported" {
		t.Fatalf("ADO relationships %+v", item)
	}
	_, err = r.Get(context.Background(), workbench.BacklogItemRequest{ID: "101"})
	if !errors.Is(err, ErrInvalidItem) {
		t.Fatalf("foreign-project read accepted: %v", err)
	}
}

type fakeClient struct {
	list func(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	get  func(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
}

func (f fakeClient) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (f fakeClient) ListWorkItems(ctx context.Context, r providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	return f.list(ctx, r)
}
func (f fakeClient) GetWorkItem(ctx context.Context, r providers.RepositoryRef, id string) (providers.WorkItem, error) {
	return f.get(ctx, r, id)
}

func TestProjectionOmissionsAndBoundedContext(t *testing.T) {
	scope, bound := source(apiv1.ProviderGitHub)
	fake := fakeClient{list: func(ctx context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no provider deadline")
		}
		if req.PageInfo == nil || req.Limit != DefaultPageItems || req.State != "all" {
			t.Fatalf("request %+v", req)
		}
		req.PageInfo.CandidateCount = 7
		items := make([]providers.WorkItem, 0, 7)
		for i := 1; i <= 7; i++ {
			id := fmt.Sprint(i)
			items = append(items, providers.WorkItem{Provider: providers.ProviderGitHub, ID: id, StableID: id, Title: "t", Body: strings.Repeat("x", 230<<10), URL: "https://github.com/org/repo/issues/" + id})
		}
		items[0].StableID = ""
		items[1].Body = strings.Repeat("x", workbench.MaxBacklogItemBytes+1)
		return items, nil
	}}
	r, _ := NewBacklogReader(scope, bound, fake)
	page, err := r.Page(context.Background(), workbench.BacklogPageRequest{})
	if err != nil || len(page.Items) != 4 || page.Omitted != 3 || !page.Exhausted || !page.Partial || !slices.Contains(page.Reasons, "page-byte-limit") {
		t.Fatalf("bounded projection %+v %v", page, err)
	}
	raw, _ := json.Marshal(page)
	if len(raw) > workbench.MaxBacklogPageBytes {
		t.Fatalf("projection size=%d", len(raw))
	}
}

func TestBacklogProviderFailureIsNotEmptySuccess(t *testing.T) {
	scope, bound := source(apiv1.ProviderGitHub)
	r, _ := NewBacklogReader(scope, bound, fakeClient{list: func(ctx context.Context, _ providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Page(ctx, workbench.BacklogPageRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read succeeded: %v", err)
	}
	for _, request := range []workbench.BacklogPageRequest{{Limit: 101}, {Limit: -1}, {Cursor: strings.Repeat("x", 2049)}, {Cursor: "https://other"}} {
		_, err = r.Page(context.Background(), request)
		if !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("invalid bounds succeeded: %+v %v", request, err)
		}
	}
}
