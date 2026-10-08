package main

import (
	"context"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
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
