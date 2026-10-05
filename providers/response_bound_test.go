package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestProjectionResponseBoundPrecedesDecode(t *testing.T) {
	payload := `[{"id":101,"number":1,"title":"` + strings.Repeat("x", 512) + `"}]`
	for _, length := range []int64{-1, int64(len(payload))} {
		t.Run(strconv.FormatInt(length, 10), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: length, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})}
			p := NewGitHubProvider("", WithHTTPClient(client))
			request := ListWorkItemsRequest{Repository: RepositoryRef{Owner: "o", Name: "r"}, Limit: 1, PageInfo: &ListWorkItemsPageInfo{}}
			_, err := p.ListWorkItems(WithResponseBodyLimit(context.Background(), 128), request)
			if !errors.Is(err, ErrResponseBodyLimit) {
				t.Fatalf("oversized response: %v", err)
			}
			items, err := p.ListWorkItems(context.Background(), request)
			if err != nil || len(items) != 1 || items[0].StableID != "101" || items[0].ID != "1" {
				t.Fatalf("ordinary read changed: %+v %v", items, err)
			}
		})
	}
}

func TestProjectionResponseBoundRejectsTrailingBytes(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: -1, Body: io.NopCloser(strings.NewReader(`[]` + strings.Repeat(" ", 100)))}, nil
	})}
	p := NewGitHubProvider("", WithHTTPClient(client))
	ctx := WithResponseBodyLimit(WithResponseBodyLimit(context.Background(), 10), 1000)
	_, err := p.ListWorkItems(ctx, ListWorkItemsRequest{Repository: RepositoryRef{Owner: "o", Name: "r"}, PageInfo: &ListWorkItemsPageInfo{}})
	if !errors.Is(err, ErrResponseBodyLimit) {
		t.Fatalf("nested bound widened or trailing bytes ignored: %v", err)
	}
}

func TestNativeWorkItemIdentityPreservesLegacyMeaning(t *testing.T) {
	gh := mapGitHubIssue(githubIssue{ID: 91, Number: 2, Milestone: &githubNode{ID: 83, Number: 3}, Assignees: []githubUser{{Login: "a"}, {Login: "b"}}})
	if gh.StableID != "91" || gh.ID != "2" || gh.ExternalID != "91" || gh.Parent.StableID != "83" || gh.Parent.ID != "3" || len(gh.NativeAssignees) != 2 {
		t.Fatalf("GitHub identity changed: %+v", gh)
	}
	ado := mapADOWorkItemState(adoWorkItem{ID: 91, Rev: 6, Fields: map[string]interface{}{"System.AssignedTo": map[string]interface{}{"displayName": "A", "uniqueName": "a@example.com"}}}, "open", WorkItemStatusOpen)
	if ado.StableID != "91" || ado.ID != "91" || ado.ExternalID != "6" || ado.Revision != "6" || len(ado.NativeAssignees) != 1 || ado.NativeAssignees[0] != "a@example.com" {
		t.Fatalf("ADO identity changed: %+v", ado)
	}
}

func TestProjectionResponseBoundAppliesToADOWIQLReadPOST(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/wiql") {
			t.Fatalf("unexpected request after refused WIQL: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: -1, Body: io.NopCloser(strings.NewReader(`{"workItems":[]}` + strings.Repeat(" ", 100)))}, nil
	})}
	p := NewADOProvider("org", "project", "", func(p *ADOProvider) { p.Client = client })
	_, err := p.ListWorkItems(WithResponseBodyLimit(context.Background(), 32), ListWorkItemsRequest{Repository: RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project"}, Limit: 1, PageInfo: &ListWorkItemsPageInfo{}})
	if !errors.Is(err, ErrResponseBodyLimit) {
		t.Fatalf("ADO WIQL bound not applied: %v", err)
	}
}
