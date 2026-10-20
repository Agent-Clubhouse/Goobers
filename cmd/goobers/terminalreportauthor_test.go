package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/providers"
)

func TestTerminalReportADOAuthorUsesOwnGUID(t *testing.T) {
	root, repo := adoConfiguredPATFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+repo.Owner+"/_apis/connectionData" {
			t.Errorf("unexpected identity route: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"authenticatedUser":{"id":"our-guid","providerDisplayName":"Shared Name"}}`))
	}))
	defer server.Close()
	old := newConfiguredADOProvider
	newConfiguredADOProvider = func(_ string, actual providers.RepositoryRef) (*providers.ADOProvider, error) {
		if actual != repo {
			t.Errorf("wrong identity connection: %+v", actual)
		}
		return providers.NewADOProvider(repo.Owner, repo.Project, "test-token", func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newConfiguredADOProvider = old })
	c := &escalationCommenter{layout: layoutFor(root)}
	own, err := c.OwnCommentMatcher(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !own(providers.Comment{AuthorID: "OUR-GUID", Author: "Renamed"}) {
		t.Fatal("our GUID rejected")
	}
	if own(providers.Comment{AuthorID: "foreign-guid", Author: "Shared Name"}) || own(providers.Comment{Author: "Shared Name"}) {
		t.Fatal("non-unique display name trusted")
	}
}

func TestTerminalReportGitHubAppAuthorUsesConfiguredIdentity(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "report-bot")
	t.Setenv("REPORT_TOKEN", "installation-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: repo.Owner + "/" + repo.Name, Env: "REPORT_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	forge := newRecordingForge(t, "")
	old := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter {
		p := providers.NewGitHubProvider(token)
		p.BaseURL = forge.server.URL
		return p
	}
	t.Cleanup(func() { newEscalationPoster = old })
	c := &escalationCommenter{layout: layoutFor(root), resolver: resolver, reg: &escTestRegistrar{}}
	own, err := c.OwnCommentMatcher(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !own(providers.Comment{Author: "REPORT-BOT[bot]"}) || own(providers.Comment{Author: "another[bot]"}) {
		t.Fatal("configured App identity not respected")
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("App identity attempted GET /user: %v", paths)
	}
}
