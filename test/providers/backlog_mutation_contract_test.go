package providerscontract

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// mutationBackend wires a stateful backend (reused from the existing
// per-provider contract fixtures) to a providers.BacklogProvider, for the
// backlog.comments/backlog.update cross-provider contracts (CONF-4, #2077).
type mutationBackend struct {
	provider providers.BacklogProvider
	repo     providers.RepositoryRef
	itemID   string
}

func newGitHubMutationBackend(t *testing.T) mutationBackend {
	t.Helper()
	b := &githubIssueBackend{labels: []string{"route/backend"}}
	srv := b.server(t)
	p := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })
	return mutationBackend{provider: p, repo: providers.RepositoryRef{Owner: "acme", Name: "app"}, itemID: "7"}
}

func newADOMutationBackend(t *testing.T) mutationBackend {
	t.Helper()
	b := newADOWorkItemBackend()
	srv := b.server(t)
	p := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = srv.URL })
	return mutationBackend{provider: p, repo: providers.RepositoryRef{Name: "repo", Project: "project"}, itemID: "42"}
}

func allMutationBackends(t *testing.T) []struct {
	name string
	b    mutationBackend
} {
	return []struct {
		name string
		b    mutationBackend
	}{
		{"github", newGitHubMutationBackend(t)},
		{"ado", newADOMutationBackend(t)},
	}
}

// TestContract_UpdateWorkItemCommentIsListable pins backlog.comments
// identically for both blessed providers: posting a comment via
// UpdateWorkItem's Comment field must make it appear, with matching body,
// in a subsequent ListComments call — the write and read paths agree on
// the same comment, regardless of provider.
func TestContract_UpdateWorkItemCommentIsListable(t *testing.T) {
	for _, tc := range allMutationBackends(t) {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b
			if _, err := b.provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
				Repository: b.repo, ID: b.itemID, Comment: "posted via contract test",
			}); err != nil {
				t.Fatalf("UpdateWorkItem: %v", err)
			}
			comments, err := b.provider.ListComments(context.Background(), b.repo, b.itemID)
			if err != nil {
				t.Fatalf("ListComments: %v", err)
			}
			found := false
			for _, c := range comments {
				if c.Body == "posted via contract test" {
					found = true
				}
			}
			if !found {
				t.Fatalf("ListComments = %#v; want the just-posted comment", comments)
			}
		})
	}
}

// TestContract_UpdateWorkItemAddLabelPersists pins backlog.update
// identically for both blessed providers: an AddLabels edit must be
// reflected on a subsequent GetWorkItem read, regardless of provider.
func TestContract_UpdateWorkItemAddLabelPersists(t *testing.T) {
	for _, tc := range allMutationBackends(t) {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b
			if _, err := b.provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
				Repository: b.repo, ID: b.itemID, AddLabels: []string{"contract-added"},
			}); err != nil {
				t.Fatalf("UpdateWorkItem: %v", err)
			}
			item, err := b.provider.GetWorkItem(context.Background(), b.repo, b.itemID)
			if err != nil {
				t.Fatalf("GetWorkItem: %v", err)
			}
			if !item.HasLabel("contract-added") {
				t.Fatalf("labels = %#v; want the just-added label", item.Labels)
			}
		})
	}
}
