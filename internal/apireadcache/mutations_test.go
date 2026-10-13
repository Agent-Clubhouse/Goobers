package apireadcache

import (
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apireadstore"
	"github.com/goobers/goobers/internal/sqliteuri"
	"github.com/goobers/goobers/providers"
)

func mutationResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func githubMutationProvider(client providers.HTTPClient) *providers.GitHubProvider {
	return providers.NewGitHubProvider("token", providers.WithHTTPClient(client))
}

func writeMutationComment(t *testing.T, client providers.HTTPClient) {
	t.Helper()
	_, err := githubMutationProvider(client).CreateWorkItemComment(t.Context(), providers.RepositoryRef{Owner: "acme", Name: "app"}, "7", "prepared")
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderMutationRefreshesAllBindingsOnlyWithinItsGaggleAndProvider(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "automation", Generation: "v1"}
	reads := 0
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return mutationResponse(201, `{"id":81,"body":"prepared"}`), nil
		}
		reads++
		return mutationResponse(200, `[]`), nil
	})
	type reader struct {
		scope Scope
		kind  providers.ProviderKind
	}
	readers := []reader{
		{scope, providers.ProviderGitHub},
		{Scope{Gaggle: "team", Binding: "interactive", Generation: "v1"}, providers.ProviderGitHub},
		{Scope{Gaggle: "team", Binding: "automation", Generation: "v2"}, providers.ProviderGitHub},
		{Scope{Gaggle: "other", Binding: "automation", Generation: "v1"}, providers.ProviderGitHub},
		{scope, providers.ProviderADO},
	}
	readAll := func() {
		t.Helper()
		for _, r := range readers {
			_ = apiReadBody(t, scopedRead(t, ScopedClient(dir, "tick", r.scope, r.kind, inner), r.kind, "token", "json"))
		}
	}
	readAll()
	readAll()
	if reads != 5 {
		t.Fatal("initial snapshots did not share", reads)
	}
	// No journal recorder is configured. A provider-confirmed write still fences reads.
	writeMutationComment(t, ScopedClient(dir, "tick", readers[1].scope, providers.ProviderGitHub, inner))
	readAll()
	readAll()
	if reads != 8 {
		t.Fatal("wrong gaggle/provider/binding invalidation", reads)
	}
}

func TestRejectedProviderMutationPreservesSnapshot(t *testing.T) {
	reads := 0
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return mutationResponse(403, `{"message":"refused"}`), nil
		}
		reads++
		return mutationResponse(200, `[]`), nil
	})
	client := ScopedClient(t.TempDir(), "tick", Scope{Gaggle: "team", Binding: "automation", Generation: "v1"}, providers.ProviderGitHub, inner)
	_ = apiReadBody(t, scopedRead(t, client, providers.ProviderGitHub, "token", "json"))
	_, err := githubMutationProvider(client).CreateWorkItemComment(t.Context(), providers.RepositoryRef{Owner: "acme", Name: "app"}, "7", "prepared")
	if err == nil {
		t.Fatal("write unexpectedly accepted")
	}
	_ = apiReadBody(t, scopedRead(t, client, providers.ProviderGitHub, "token", "json"))
	if reads != 1 {
		t.Fatal("rejected write invalidated snapshot", reads)
	}
}

func TestADOMutationRefreshesDeclaredPostReadPlans(t *testing.T) {
	dir := t.TempDir()
	scope := Scope{Gaggle: "team", Binding: "backlog", Generation: "v1"}
	fixture := &adoPlanFixture{}
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/comments") {
			return mutationResponse(200, `{"id":81,"text":"prepared"}`), nil
		}
		return fixture.Do(req)
	})
	p := adoPlanProvider(dir, "tick", scope, "token", inner)
	read := func() {
		t.Helper()
		items, info, err := adoPlanRead(t.Context(), p)
		assertADOPlanItems(t, items, info, err)
	}
	read()
	read()
	if q, b := fixture.counts(); q != 1 || b != 1 {
		t.Fatalf("POST reads invalidated themselves: %d/%d", q, b)
	}
	if _, err := p.CreateWorkItemComment(t.Context(), providers.RepositoryRef{Project: "project"}, "1", "prepared"); err != nil {
		t.Fatal(err)
	}
	read()
	read()
	if q, b := fixture.counts(); q != 2 || b != 2 {
		t.Fatalf("mutation did not fence both read plans: %d/%d", q, b)
	}
}

func TestMutationCacheFailurePreservesWriteAndBypassesReuse(t *testing.T) {
	dir := t.TempDir()
	reads := 0
	inner := scopedTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return mutationResponse(201, `{"id":81,"body":"prepared"}`), nil
		}
		reads++
		return mutationResponse(200, `[]`), nil
	})
	client := ScopedClient(dir, "tick", Scope{Gaggle: "team", Binding: "automation", Generation: "v1"}, providers.ProviderGitHub, inner)
	read := func() { _ = apiReadBody(t, scopedRead(t, client, providers.ProviderGitHub, "token", "json")) }
	read()
	read()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(dir, apireadstore.FileName)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER reject_mutation_generation BEFORE UPDATE ON snapshot_generations BEGIN SELECT RAISE(FAIL, 'injected cache failure'); END`); err != nil {
		t.Fatal(err)
	}
	writeMutationComment(t, client)
	read()
	if _, err := db.Exec(`DROP TRIGGER reject_mutation_generation`); err != nil {
		t.Fatal(err)
	}
	read()
	if reads != 3 {
		t.Fatal("failed invalidation reused stale snapshot", reads)
	}
}
