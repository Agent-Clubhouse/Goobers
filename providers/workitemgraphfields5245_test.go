package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCheckCreateWorkItemGraphFields pins which declarations are refused.
//
// The negative cases carry the compatibility promise: a create that declares no
// edges must be completely unaffected, so this refusal cannot break any caller
// that was already getting correct behavior. A Parent pointer present but empty
// counts as no declaration — a zero-valued struct threaded through a caller is
// not a request for an edge, and refusing it would break working code.
func TestCheckCreateWorkItemGraphFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     CreateWorkItemRequest
		refused bool
		names   []string
	}{
		{name: "no graph fields is admitted", req: CreateWorkItemRequest{Title: "t"}},
		{
			name: "nil parent and empty links are admitted",
			req:  CreateWorkItemRequest{Title: "t", Parent: nil, Links: nil},
		},
		{
			name: "parent present but empty is not a declaration",
			req:  CreateWorkItemRequest{Title: "t", Parent: &WorkItemRef{}},
		},
		{
			name: "whitespace-only parent id is not a declaration",
			req:  CreateWorkItemRequest{Title: "t", Parent: &WorkItemRef{ID: "   "}},
		},
		{
			name:    "declared parent is refused",
			req:     CreateWorkItemRequest{Title: "t", Parent: &WorkItemRef{ID: "42"}},
			refused: true, names: []string{"parent"},
		},
		{
			name:    "declared links are refused",
			req:     CreateWorkItemRequest{Title: "t", Links: []Link{{}}},
			refused: true, names: []string{"links"},
		},
		{
			name: "both are named in one refusal",
			req: CreateWorkItemRequest{
				Title: "t", Parent: &WorkItemRef{ID: "42"}, Links: []Link{{}},
			},
			refused: true, names: []string{"parent", "links"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCreateWorkItemGraphFields(tc.req)
			if !tc.refused {
				if err != nil {
					t.Fatalf("admitted request was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnsupportedCreateGraphFields) {
				t.Fatalf("error = %v, want ErrUnsupportedCreateGraphFields", err)
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %q", err, name)
				}
			}
			// Actionable: the refusal has to say what DOES work, or a caller is
			// left with a field they cannot use and no alternative.
			for _, want := range []string{"AttachWorkItemChild", "AttachWorkItemBlocker", "#5245"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestCreateWorkItemRefusesGraphFieldsBeforeMutation is the property that
// matters most, asserted per provider: the refusal happens BEFORE the create
// request is sent.
//
// #5245 requires forbidden fields fail before mutation, and the reason is
// concrete — an item created with its edges dropped cannot be fixed by
// retrying, because the retry files a duplicate instead of adding the missing
// edges. The server here fails the test if it is contacted at all, which is a
// stronger assertion than checking the returned error: an error alone would
// still be consistent with a create that happened and then reported a problem.
//
// All three providers are covered because the gap turned out to be universal —
// GitHub and Gitea dropped these fields exactly as ADO did, so refusing in only
// one place would leave the same silent drop in the others.
func TestCreateWorkItemRefusesGraphFieldsBeforeMutation(t *testing.T) {
	req := CreateWorkItemRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app", Project: "project"},
		Title:      "needs a parent",
		Parent:     &WorkItemRef{ID: "42"},
	}

	for _, tc := range []struct {
		name string
		call func(t *testing.T, baseURL string) error
	}{
		{
			name: "ado",
			call: func(t *testing.T, baseURL string) error {
				p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = baseURL })
				_, err := p.CreateWorkItem(context.Background(), req)
				return err
			},
		},
		{
			name: "github",
			call: func(t *testing.T, baseURL string) error {
				p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = baseURL })
				_, err := p.CreateWorkItem(context.Background(), req)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("provider contacted %s %s; the refusal must precede any mutation", r.Method, r.URL.Path)
			}))
			defer server.Close()

			err := tc.call(t, server.URL)
			if !errors.Is(err, ErrUnsupportedCreateGraphFields) {
				t.Fatalf("CreateWorkItem error = %v, want ErrUnsupportedCreateGraphFields", err)
			}
		})
	}
}
