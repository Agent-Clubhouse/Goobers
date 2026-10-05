package workbenchservice

import (
	"context"
	"crypto/sha1" // Native Git object fixture.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func documentsFixture(t *testing.T, transport roundTrip) (*Service, apiv1.Gaggle, httpapi.Principal) {
	t.Helper()
	service, g, p := fixture(t, transport)
	target := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "acme", Name: "code"}
	g.Spec.Project.Branch = "strategy"
	g.Spec.Workbench.Sources = []apiv1.WorkbenchSource{{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}}}
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	g.Spec.InteractiveAccess.Credentials = apiv1.InteractiveCredentialBindings{Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: target, CredentialRef: "human-repo"}}}
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, repositoryCredentials(), interactiveaccess.Dependencies{Registrar: &secretRegistry{}})
	if err != nil {
		t.Fatal(err)
	}
	service.Permissions = permissions
	factory := ProviderFactory{SchedulerDirectory: t.TempDir(), Client: &http.Client{Transport: transport}, Registrar: &secretRegistry{}}
	service.Repository = factory.Repository
	return service, g, p
}
func repositoryCredentials() []instance.InteractiveCredential {
	return []instance.InteractiveCredential{{Name: "human-repo", Provider: "github", Owner: "acme", Repository: "code", Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}}}
}

const documentSource = "---\ngoobers:\n  schemaVersion: objectives/v1\n  objectiveId: obj-00000000-0000-0000-0000-000000000001\n  title: Durable source objective\n---\n# Native source\n"

func sourceResponses(t *testing.T, commit string) map[string]string {
	t.Helper()
	h := sha1.New() //nolint:gosec // Native Git object fixture.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(documentSource))
	_, _ = h.Write([]byte(documentSource))
	blob, tree := hex.EncodeToString(h.Sum(nil)), strings.Repeat("b", 40)
	values := map[string]any{
		"/repos/acme/code/git/ref/heads/strategy": map[string]any{"ref": "refs/heads/strategy", "object": map[string]string{"sha": commit}},
		"/repos/acme/code/git/commits/" + commit:  map[string]any{"sha": commit, "tree": map[string]string{"sha": tree}},
		"/repos/acme/code/git/trees/" + tree:      map[string]any{"sha": tree, "tree": []map[string]any{{"path": "plan.md", "type": "blob", "mode": "100644", "sha": blob, "size": len(documentSource)}}},
		"/repos/acme/code/git/blobs/" + blob:      map[string]any{"sha": blob, "size": len(documentSource), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(documentSource))},
	}
	result := map[string]string{}
	for path, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = string(raw)
	}
	return result
}

func TestDocumentsUseExactInteractiveRepositoryAndConditionalCache(t *testing.T) {
	commit := strings.Repeat("a", 40)
	responses := sourceResponses(t, commit)
	seen := map[string]int{}
	token := "human-read-canary"
	service, g, p := documentsFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Host != "api.github.com" {
			t.Fatalf("wrong scoped request: %s", r.URL)
		}
		body, ok := responses[r.URL.Path]
		if !ok {
			t.Fatalf("undeclared request: %s", r.URL)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("unbounded read")
		}
		key := token + r.URL.Path
		seen[key]++
		if seen[key] > 1 {
			if r.Header.Get("If-None-Match") != `"revision-1"` {
				t.Fatal("no conditional revalidation")
			}
			return issueResponse(r, 304, ""), nil
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Fatal("cache crossed credential visibility")
		}
		return issueResponse(r, 200, body), nil
	})
	for range 2 {
		page, err := service.Documents(context.Background(), p, g.Name, "strategy", workbench.DocumentPageRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if page.Coverage != "complete" || len(page.Files) != 1 || page.Files[0].Ref == nil || page.Files[0].Provenance.Commit != commit || page.Files[0].Body != "# Native source\n" {
			t.Fatalf("page=%+v", page)
		}
	}
	for _, count := range seen {
		if count != 2 {
			t.Fatal("missing revalidation", seen)
		}
	}
	token = "rotated-human-token"
	t.Setenv("HUMAN_BACKLOG", token)
	if _, err := service.Documents(context.Background(), p, g.Name, "strategy", workbench.DocumentPageRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 8 {
		t.Fatalf("credential scopes not separate: %+v", seen)
	}
}

func TestDocumentsDenyBeforeProviderAndDoNotBorrowAutomationCredentials(t *testing.T) {
	var calls atomic.Int32
	service, g, p := documentsFixture(t, func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("provider-secret") })
	stranger := p
	stranger.Subject = "mallory"
	for _, tc := range []struct {
		principal       httpapi.Principal
		gaggle, binding string
	}{{stranger, g.Name, "strategy"}, {p, "another-gaggle", "strategy"}, {p, g.Name, "items"}, {p, g.Name, "https://foreign/repo"}} {
		if _, err := service.Documents(context.Background(), tc.principal, tc.gaggle, tc.binding, workbench.DocumentPageRequest{}); err == nil {
			t.Fatal("unauthorized read")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("denied requests reached provider")
	}
	if _, err := service.Documents(context.Background(), p, g.Name, "strategy", workbench.DocumentPageRequest{}); err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatal("unsafe provider error", err)
	}
	before := calls.Load()
	t.Setenv("HUMAN_BACKLOG", "")
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := service.Documents(context.Background(), p, g.Name, "strategy", workbench.DocumentPageRequest{})
	var public *httpapi.InterventionError
	if !errors.As(err, &public) || public.Status != 503 || calls.Load() != before {
		t.Fatalf("missing exact credential used fallback: %v", err)
	}
}

func TestDocumentsBranchMoveRequiresFreshScanAndCancellationDropsContent(t *testing.T) {
	commit := strings.Repeat("a", 40)
	responses := sourceResponses(t, commit)
	service, g, p := documentsFixture(t, func(r *http.Request) (*http.Response, error) {
		return issueResponse(r, 200, responses[r.URL.Path]), nil
	})
	g.Spec.Workbench.Sources[0].Paths = []string{"plan.md", "later.md"}
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	page, err := service.Documents(context.Background(), p, g.Name, "strategy", workbench.DocumentPageRequest{Limit: 1})
	if err != nil || page.NextCursor == "" || page.Coverage != "partial" {
		t.Fatalf("%+v %v", page, err)
	}
	responses = sourceResponses(t, strings.Repeat("c", 40))
	got, err := service.Documents(context.Background(), p, g.Name, "strategy", workbench.DocumentPageRequest{Limit: 1, Cursor: page.NextCursor})
	var public *httpapi.InterventionError
	if !errors.As(err, &public) || public.Status != 409 || len(got.Files) != 0 {
		t.Fatalf("historical cursor accepted: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = service.Documents(ctx, p, g.Name, "strategy", workbench.DocumentPageRequest{})
	if err == nil || len(got.Files) != 0 {
		t.Fatal("cancelled read leaked content", got, err)
	}
}

func TestRepositoryFactoryUsesOnlyExactDeliveredADOCredential(t *testing.T) {
	target := apiv1.InteractiveRepositoryIdentity{Provider: "ado", Owner: "organization", Project: "project", Name: "wiki"}
	binding := ReadBinding{Scope: workbench.Scope{GaggleID: "g", Bindings: map[string]bool{"wiki": true}}, Source: workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: "wiki", Repository: &target}}, Generation: "applied"}
	f := ProviderFactory{Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		_, password, ok := r.BasicAuth()
		if !ok || password != "exact-pat" || !strings.HasPrefix(r.URL.Path, "/organization/project/_apis/git/repositories/wiki/") {
			t.Fatal("wrong ADO target or auth", r.URL)
		}
		return issueResponse(r, 200, `{"value":[{"name":"refs/heads/main","objectId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`), nil
	})}, Registrar: &secretRegistry{}}
	client, err := f.Repository(context.Background(), binding, interactiveaccess.Credential{Value: "exact-pat", Scheme: "basic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.ReadSourceBranch(context.Background(), providers.RepositoryRef{Provider: providers.ProviderADO, Owner: target.Owner, Project: target.Project, Name: target.Name}, "main"); err != nil {
		t.Fatal(err)
	}
}
