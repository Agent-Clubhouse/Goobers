package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// escReaderCommenter is an escFakeCommenter that can also read the item, as
// the real GitHub provider can.
type escReaderCommenter struct {
	escFakeCommenter
	gotRepo providers.RepositoryRef
	gotID   string
	labels  []string
}

func (f *escReaderCommenter) GetWorkItem(_ context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error) {
	f.gotRepo, f.gotID = repo, id
	return providers.WorkItem{ID: id, Labels: f.labels}, nil
}

// TestEscalationCommenterReadsItemLabels covers #5430's production seam: the
// failure-streak comment learns which park label holds the item through the
// same per-call token resolution as every other escalation write, with the
// pr/<n> claim key mapped to the provider's bare number.
func TestEscalationCommenterReadsItemLabels(t *testing.T) {
	t.Setenv("ESC_5430_TOK", "token-5430")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "ESC_5430_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	fake := &escReaderCommenter{labels: []string{providers.LabelMergeEscalated}}
	var gotToken string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter { gotToken = token; return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	var reader gate.WorkItemReader = &escalationCommenter{resolver: resolver, reg: &escTestRegistrar{}}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	item, err := reader.GetWorkItem(context.Background(), repo, "pr/42")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if gotToken != "token-5430" || fake.gotRepo != repo || fake.gotID != "42" {
		t.Fatalf("read via token=%q repo=%+v id=%q, want token-5430 acme/web 42", gotToken, fake.gotRepo, fake.gotID)
	}
	if !slices.Equal(item.Labels, []string{providers.LabelMergeEscalated}) {
		t.Fatalf("labels = %v", item.Labels)
	}

	newEscalationPoster = func(string) gate.Commenter { return &escFakeCommenter{} }
	if _, err := reader.GetWorkItem(context.Background(), repo, "42"); err == nil {
		t.Fatal("GetWorkItem through a poster that cannot read items succeeded; want an error so the comment hedges")
	}
}

// TestEscalationCommenterNamesADOMergeEscalatedLabel: an ADO merge-review PR
// parked by merge-escalated gets a failure-streak comment naming that label,
// read from the PR's labels endpoint rather than a Boards work item.
func TestEscalationCommenterNamesADOMergeEscalatedLabel(t *testing.T) {
	var (
		mu      sync.Mutex
		labelOK bool
		posted  []string
	)
	// One permissive body serves every Boards read the comment post makes (the
	// work item, its type's states, its comments).
	const adoRead = `{"id":42,"rev":1,"fields":{"System.WorkItemType":"Issue","System.State":"To Do"},"value":[{"name":"To Do","category":"Proposed"}],"comments":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pullrequests/42/labels"):
			labelOK = true
			_, _ = io.WriteString(w, `{"value":[{"id":"1","name":"`+providers.LabelMergeEscalated+`"}]}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, adoRead)
		default:
			body, _ := io.ReadAll(r.Body)
			posted = append(posted, string(body))
			_, _ = io.WriteString(w, adoRead)
		}
	}))
	t.Cleanup(server.Close)
	prev := newConfiguredADOProvider
	newConfiguredADOProvider = func(string, providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider("example-org", "proj", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newConfiguredADOProvider = prev })

	commenter := &escalationCommenter{reg: &escTestRegistrar{}, layout: instance.NewLayout(t.TempDir()).ForGaggle("example")}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "proj", Name: "repo"}
	if err := gate.UpsertFailureComment(providers.WithAttributionContext(context.Background(), providers.Attribution{Workflow: "merge-review", Run: "run-1"}), commenter, repo, "pr/42", 3, "merge-review", "run-1", "https://example.invalid/run-1", nil); err != nil {
		t.Fatalf("UpsertFailureComment: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "Remove `" + providers.LabelMergeEscalated + "` and re-approve to retry."
	if !labelOK || len(posted) != 1 || !strings.Contains(posted[0], want) {
		t.Fatalf("labels read=%v posted=%q, want one comment containing %q", labelOK, posted, want)
	}
}
