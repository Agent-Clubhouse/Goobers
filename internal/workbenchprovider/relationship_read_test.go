package workbenchprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func TestSelectedGitHubRelationshipsVerifyScopedStableIDsAndBoundRequests(t *testing.T) {
	calls := []string{}
	parentMissing := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.URL.Path)
		if request.Header.Get("Authorization") != "Bearer human" {
			t.Fatal("wrong identity")
		}
		switch request.URL.Path {
		case "/repos/org/repo/issues/7":
			return response(`{"id":101,"number":7,"title":"Selected","state":"open","html_url":"https://github.com/org/repo/issues/7","milestone":{"id":501,"number":2,"title":"v1"}}`), nil
		case "/repos/org/repo/issues/7/parent":
			if parentMissing {
				r := response(`{"message":"Not Found"}`)
				r.StatusCode = 404
				return r, nil
			}
			return response(`{"id":202,"number":8,"title":"Parent","state":"open","html_url":"https://github.com/org/repo/issues/8"}`), nil
		case "/repos/org/repo/issues/7/dependencies/blocked_by":
			if request.URL.Query().Get("per_page") != "64" || request.URL.Query().Get("page") != "1" {
				t.Fatal("unbounded dependencies")
			}
			r := response(`[{"id":303,"number":9,"state":"open","html_url":"https://github.com/org/repo/issues/9"},{"id":404,"number":10,"state":"open","html_url":"https://github.com/foreign/repo/issues/10"}]`)
			r.Header.Set("Link", `<https://api.github.com/next>; rel="next"`)
			return r, nil
		default:
			t.Fatalf("extra request %s", request.URL)
			return nil, nil
		}
	})}
	scope, bound := source(apiv1.ProviderGitHub)
	reader, err := NewBacklogReader(scope, bound, providers.NewGitHubProvider("human", providers.WithHTTPClient(client)))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := reader.Get(t.Context(), workbench.BacklogItemRequest{ID: "7"})
	if err != nil || len(calls) != 1 || plain.RelationshipCoverage.Parents != "not-loaded" {
		t.Fatal(plain, calls, err)
	}
	item, err := reader.GetWithRelationships(t.Context(), workbench.BacklogItemRequest{ID: "7"})
	if err != nil || len(calls) != 4 || len(item.Relationships) != 4 {
		t.Fatal(item, calls, err)
	}
	if item.RelationshipCoverage.Parents != "complete" || item.RelationshipCoverage.Blockers != "partial" || item.Relationships[0].Kind != "milestone-member" || item.Relationships[1].Target.Ref.SourceID != "202" || item.Relationships[2].Target.Ref.SourceID != "303" || item.Relationships[3].Target.Ref != nil {
		t.Fatal("relation conflation or foreign scope", item)
	}
	parentMissing = true
	item, err = reader.GetWithRelationships(t.Context(), workbench.BacklogItemRequest{ID: "7"})
	if err != nil || item.RelationshipCoverage.Parents != "partial" || len(calls) != 7 {
		t.Fatal("404 proved missing parent", item, calls, err)
	}
}

func TestSelectedADORelationshipsUseOneBoundedProjectVerifiedBatch(t *testing.T) {
	batches := 0
	var ids []int
	links := make([]map[string]string, 0, 70)
	for id := 1; id <= 70; id++ {
		kind := "System.LinkTypes.Dependency-Reverse"
		if id == 1 {
			kind = "System.LinkTypes.Hierarchy-Reverse"
		}
		links = append(links, map[string]string{"rel": kind, "url": fmt.Sprintf("https://dev.azure.com/org/_apis/wit/workItems/%d", id)})
	}
	raw, _ := json.Marshal(links)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/workitems/101"):
			return response(`{"id":101,"rev":1,"url":"https://dev.azure.com/org/_apis/wit/workItems/101","fields":{"System.TeamProject":"project","System.WorkItemType":"Epic","System.State":"Active","System.Title":"Selected"},"relations":` + string(raw) + `}`), nil
		case strings.Contains(request.URL.Path, "/workitemtypes/Epic/states"):
			return response(`{"value":[{"name":"Active","category":"InProgress"}]}`), nil
		case strings.HasSuffix(request.URL.Path, "/workitemsbatch"):
			batches++
			var body struct {
				IDs []int `json:"ids"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			ids = body.IDs
			return response(`{"value":[{"id":1,"url":"https://dev.azure.com/org/_apis/wit/workItems/1","fields":{"System.TeamProject":"project"}},{"id":2,"url":"https://dev.azure.com/org/_apis/wit/workItems/2","fields":{"System.TeamProject":"other"}},{"id":3,"url":"https://dev.azure.com/org/_apis/wit/workItems/3","fields":{"System.TeamProject":"project"}},{"id":4,"url":"https://dev.azure.com/org/_apis/wit/workItems/4","fields":{"System.TeamProject":"other"}},{"id":4,"url":"https://dev.azure.com/org/_apis/wit/workItems/4","fields":{"System.TeamProject":"project"}},{"id":999,"url":"https://dev.azure.com/org/_apis/wit/workItems/999","fields":{"System.TeamProject":"project"}}]}`), nil
		default:
			t.Fatalf("relation target caused additional request %s", request.URL)
			return nil, nil
		}
	})}
	scope, bound := source(apiv1.ProviderADO)
	provider := providers.NewADOProvider("org", "project", "human")
	provider.Client = client
	reader, err := NewBacklogReader(scope, bound, provider)
	if err != nil {
		t.Fatal(err)
	}
	item, err := reader.GetWithRelationships(t.Context(), workbench.BacklogItemRequest{ID: "101"})
	if err != nil || batches != 1 || len(ids) != 64 || len(item.Relationships) != 64 {
		t.Fatal(item, batches, ids, err)
	}
	if item.RelationshipCoverage.Parents != "complete" || item.RelationshipCoverage.Blockers != "partial" || item.Relationships[0].Target.Ref.SourceID != "1" || item.Relationships[1].Target.Ref != nil || item.Relationships[2].Target.Ref.SourceID != "3" || item.Relationships[3].Target.Ref != nil {
		t.Fatal("project membership not enforced", item)
	}
}

func TestSelectedRelationshipsCancellationCannotEscapeReadOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/parent") {
			cancel()
			return nil, request.Context().Err()
		}
		return response(`{"id":101,"number":7,"title":"Selected","state":"open","html_url":"https://github.com/org/repo/issues/7"}`), nil
	})}
	scope, bound := source(apiv1.ProviderGitHub)
	reader, err := NewBacklogReader(scope, bound, providers.NewGitHubProvider("", providers.WithHTTPClient(client)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.GetWithRelationships(ctx, workbench.BacklogItemRequest{ID: "7"}); err == nil {
		t.Fatal("cancelled authority returned completed relation read")
	}
}
