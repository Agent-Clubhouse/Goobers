package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

// newGitHubParentServer serves GET /repos/{owner}/{repo}/issues/{n}/parent
// from parents (path -> parent issue JSON); any other issue has no parent
// (404, as GitHub answers). denied paths answer 403.
func newGitHubParentServer(t *testing.T, parents map[string]map[string]interface{}, denied map[string]bool) (*GitHubProvider, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		paths = append(paths, r.URL.Path)
		if denied[r.URL.Path] {
			http.Error(w, `{"message":"Must have admin rights to Repository."}`, http.StatusForbidden)
			return
		}
		parent, ok := parents[r.URL.Path]
		if !ok {
			http.Error(w, `{"message":"No parent issue found"}`, http.StatusNotFound)
			return
		}
		writeJSON(t, w, parent)
	}))
	t.Cleanup(server.Close)
	return NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }), &paths
}

func githubParentJSON(number int, repo, typeName, body string) map[string]interface{} {
	issue := map[string]interface{}{
		"id": number * 10, "number": number, "title": "Parent", "body": body, "state": "open",
		"html_url":       "https://github.example/" + repo + "/issues/" + strconv.Itoa(number),
		"repository_url": "https://api.github.example/repos/" + repo,
	}
	if typeName != "" {
		issue["type"] = map[string]interface{}{"name": typeName}
	}
	return issue
}

// TestGitHubAncestryReadsNativeSubIssueParents: a GitHub issue's parent comes
// from the sub-issues parent endpoint, one call per level; its issue type is
// the work-item type; the chain ends at the 404 GitHub answers for an issue
// with no parent, which is not an omission.
func TestGitHubAncestryReadsNativeSubIssueParents(t *testing.T) {
	provider, paths := newGitHubParentServer(t, map[string]map[string]interface{}{
		"/repos/acme/app/issues/7/parent": githubParentJSON(3, "acme/app", "Feature", "Feature body"),
		"/repos/acme/app/issues/3/parent": githubParentJSON(1, "acme/app", "", "Epic body"),
	}, nil)
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	root := provider.AncestryRoot(repo, WorkItem{ID: "7"})
	got, err := TraverseWorkItemAncestry(context.Background(), provider, repo, []WorkItemNode{root}, AncestryOptions{MaxDepth: 5, MaxItems: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github:acme/app:3", "github:acme/app:1"}; !reflect.DeepEqual(ancestorKeys(got), want) {
		t.Fatalf("items = %v, want %v", ancestorKeys(got), want)
	}
	if got.Items[0].Type != "Feature" || got.Items[1].Type != "issue" || got.Items[0].Fields[0] != (WorkItemField{Name: "body", Value: "Feature body"}) {
		t.Fatalf("items = %+v", got.Items)
	}
	if got.Status != AncestryComplete || len(*paths) != 3 {
		t.Fatalf("status %q after %v, want complete after three parent reads", got.Status, *paths)
	}
}

func TestGitHubAncestryCrossRepositoryParentAndDeniedRead(t *testing.T) {
	provider, _ := newGitHubParentServer(t, map[string]map[string]interface{}{
		"/repos/acme/app/issues/7/parent":   githubParentJSON(3, "acme/other", "Epic", "x"),
		"/repos/acme/other/issues/3/parent": githubParentJSON(2, "acme/other", "", "y"),
	}, map[string]bool{"/repos/acme/other/issues/2/parent": true})
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	root := provider.AncestryRoot(repo, WorkItem{ID: "7"})

	denied, err := TraverseWorkItemAncestry(context.Background(), provider, repo, []WorkItemNode{root}, AncestryOptions{MaxDepth: 5, MaxItems: 10, CrossProject: AncestryCrossProjectDeny})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github:acme/app:7>github:acme/other:3@cross-project"}; !reflect.DeepEqual(omissionReasons(denied), want) || len(denied.Items) != 0 {
		t.Fatalf("deny = %v %v, want %v", ancestorKeys(denied), omissionReasons(denied), want)
	}

	allowed, err := TraverseWorkItemAncestry(context.Background(), provider, repo, []WorkItemNode{root}, AncestryOptions{MaxDepth: 5, MaxItems: 10, CrossProject: AncestryCrossProjectAllow})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github:acme/other:3", "github:acme/other:2"}; !reflect.DeepEqual(ancestorKeys(allowed), want) {
		t.Fatalf("allow items = %v, want %v", ancestorKeys(allowed), want)
	}
	if len(allowed.Omissions) != 1 || allowed.Omissions[0].Reason != AncestryOmitAccessDenied || allowed.Omissions[0].Child != "github:acme/other:2" {
		t.Fatalf("allow omissions = %+v, want access-denied reading issue 2's parent", allowed.Omissions)
	}
}

// A child with no owner/name repository is a failed read made without a
// request, and a field name GitHub has no equivalent for selects nothing.
func TestGitHubAncestryChildWithoutRepositoryAndUnknownFields(t *testing.T) {
	provider, paths := newGitHubParentServer(t, map[string]map[string]interface{}{
		"/repos/acme/app/issues/7/parent": githubParentJSON(3, "acme/app", "Feature", "Feature body"),
	}, nil)
	reads, err := provider.ReadWorkItemParents(context.Background(), RepositoryRef{}, []WorkItemNode{
		{Provider: ProviderGitHub, Project: "no-slash", ID: "1", ParentUnknown: true},
		{Provider: ProviderGitHub, Project: "acme/app", ID: "7", ParentUnknown: true},
	}, []string{"System.Description", "Body"})
	if err != nil {
		t.Fatal(err)
	}
	if r := reads[0]; r.Omission != AncestryOmitReadFailed || r.Detail != "child has no owner/name repository" || r.Parent != nil {
		t.Errorf("read 0 = %+v, want a failed read naming the missing repository", r)
	}
	if want := []string{"/repos/acme/app/issues/7/parent"}; !reflect.DeepEqual(*paths, want) {
		t.Errorf("requests = %v, want only the readable child's parent", *paths)
	}
	if r := reads[1]; r.Parent == nil || !reflect.DeepEqual(r.Parent.Fields, []WorkItemField{{Name: "Body", Value: "Feature body"}}) {
		t.Errorf("read 1 = %+v, want only the body field selected", r)
	}
}

// A walk whose context has ended returns that error from the first child.
func TestGitHubAncestryCancelledContextIsAnError(t *testing.T) {
	provider, _ := newGitHubParentServer(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads, err := provider.ReadWorkItemParents(ctx, RepositoryRef{}, []WorkItemNode{{Provider: ProviderGitHub, Project: "acme/app", ID: "7", ParentUnknown: true}}, nil)
	if !errors.Is(err, context.Canceled) || reads != nil {
		t.Fatalf("ReadWorkItemParents = %+v, %v; want the cancellation", reads, err)
	}
}
