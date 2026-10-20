package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type adoTerminalLabelsFixture struct {
	mu      sync.Mutex
	label   string
	posts   int
	deleted []string
	headers []string
	reject  bool
}

func (f *adoTerminalLabelsFixture) server(t *testing.T, repo providers.RepositoryRef) *httptest.Server {
	t.Helper()
	pulls := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	labels := pulls + "/42/labels"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = append(f.headers, r.Header.Get("Authorization"))
		if f.reject {
			http.Error(w, "forbidden "+r.Header.Get("Authorization"), http.StatusForbidden)
			return
		}
		value := []map[string]string{}
		if f.label != "" {
			value = append(value, map[string]string{"id": "server-label-id", "name": f.label})
		}
		switch {
		case r.URL.Path == pulls && r.Method == http.MethodGet:
			if r.URL.Query().Get("includeLabels") != "true" {
				t.Error("PR listing omitted includeLabels=true")
			}
			writeFakeJSON(w, map[string]any{"value": []map[string]any{{"pullRequestId": 42, "status": "active", "labels": value}}})
		case r.URL.Path == labels:
			if r.URL.Query().Get("api-version") != "7.1-preview.1" {
				t.Errorf("label API version=%s", r.URL.RawQuery)
			}
			switch r.Method {
			case http.MethodGet:
				writeFakeJSON(w, map[string]any{"value": value})
			case http.MethodPost:
				var body struct {
					Name string `json:"name"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				f.posts++
				f.label = body.Name
				writeFakeJSON(w, map[string]string{"id": "server-label-id", "name": f.label})
			default:
				t.Errorf("unexpected label method %s", r.Method)
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
		case r.URL.Path == labels+"/server-label-id" && r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, r.URL.Path)
			f.label = ""
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected endpoint %s %s (must use Git PR labels, never Boards)", r.Method, r.URL.Path)
			http.Error(w, "unexpected endpoint", 404)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func pointTerminalADOLabelerAt(t *testing.T, url string) {
	t.Helper()
	original := newADORunAbortLabelProvider
	newADORunAbortLabelProvider = func(repo providers.RepositoryRef, kind string, source providers.TokenSource, registrar providers.SecretRegistrar) (workItemUpdater, error) {
		updater, err := original(repo, kind, source, registrar)
		if err != nil {
			return nil, err
		}
		native := updater.(adoTerminalPRLabelUpdater)
		native.BaseURL = url
		return native, nil
	}
	t.Cleanup(func() { newADORunAbortLabelProvider = original })
}

func TestADOTerminalAbortLabelUsesNativeLabelsAndIsVisibleToSelection(t *testing.T) {
	t.Setenv("TERMINAL_ADO_PAT", "terminal-label-pat")
	cfg := adoTerminalConfig(instance.ADOAuthPAT)
	repo := terminalRepositoryRefForProject(cfg, apiv1.RepoRef{})
	fixture := &adoTerminalLabelsFixture{}
	server := fixture.server(t, repo)
	pointTerminalADOLabelerAt(t, server.URL)
	label, err := buildTerminalRunAbortLabeler(cfg, apiv1.RepoRef{}, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runsDir, runID, jr := newRunAbortLabelJournal(t, "")
	if err := jr.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "ado", Kind: "pr", ID: "42"}, Runner: map[string]any{"operation": prOpenOperation}}); err != nil {
		t.Fatal(err)
	}
	if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, label); err != nil {
		t.Fatal(err)
	}
	if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, label); err != nil {
		t.Fatal(err)
	}
	reader := providers.NewADOProvider(repo.Owner, repo.Project, "terminal-label-pat", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	prs, err := reader.ListPullRequests(context.Background(), providers.ListPullRequestsRequest{Repository: repo, SkipCheckState: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || !slices.Contains(prs[0].Labels, abortedRunLabel) {
		t.Fatalf("selection cannot see abort label: %+v", prs)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.posts != 1 || fixture.label != abortedRunLabel {
		t.Fatalf("posts=%d label=%s", fixture.posts, fixture.label)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:terminal-label-pat"))
	for _, header := range fixture.headers {
		if header != want {
			t.Errorf("unexpected auth scheme")
		}
	}
}

func TestADOTerminalLabelAdapterCaseFoldAndDeleteByID(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	fixture := &adoTerminalLabelsFixture{label: strings.ToUpper(abortedRunLabel)}
	server := fixture.server(t, repo)
	p := providers.NewADOProvider(repo.Owner, repo.Project, "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	adapter := adoTerminalPRLabelUpdater{p}
	if _, err := adapter.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{Repository: repo, ID: "42", AddLabels: []string{abortedRunLabel}}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{Repository: repo, ID: "42", RemoveLabels: []string{abortedRunLabel}}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.posts != 0 || len(fixture.deleted) != 1 || !strings.HasSuffix(fixture.deleted[0], "/server-label-id") || fixture.label != "" {
		t.Fatalf("unexpected mutation: %+v", fixture)
	}
}

func TestADOTerminalLabelUsesDeclaredCapabilityPATOverEntraRepository(t *testing.T) {
	t.Setenv("EXAMPLE_OVERRIDE_PAT", "label-override-pat")
	cfg := adoEntraGaggleWithPATOverride(string(capability.ProviderPRWrite))
	repo := terminalRepositoryRefForProject(cfg, apiv1.RepoRef{})
	fixture := &adoTerminalLabelsFixture{}
	server := fixture.server(t, repo)
	pointTerminalADOLabelerAt(t, server.URL)
	reg := journal.NewRegistryScrubber()
	label, err := buildTerminalRunAbortLabeler(cfg, apiv1.RepoRef{}, reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := label(context.Background(), providers.UpdateWorkItemRequest{Repository: repo, ID: "42", AddLabels: []string{abortedRunLabel}}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:label-override-pat"))
	for _, header := range fixture.headers {
		if header != want {
			t.Errorf("capability override used wrong auth scheme")
		}
	}
	fixture.reject = true
	fixture.mu.Unlock()
	_, err = label(context.Background(), providers.UpdateWorkItemRequest{Repository: repo, ID: "42", AddLabels: []string{abortedRunLabel}})
	if err == nil || strings.Contains(err.Error(), "label-override-pat") || strings.Contains(err.Error(), want) {
		t.Fatalf("provider error not scrubbed: %v", err)
	}
}

func TestADOTerminalLabelScopesToSelectedRepository(t *testing.T) {
	t.Setenv("TERMINAL_ADO_PAT", "selected-repo-pat")
	t.Setenv("UNRELATED_REPO_PAT", "unrelated-pat")
	cfg := adoTerminalConfig(instance.ADOAuthPAT)
	selected := cfg.Repos[0]
	cfg.Repos = append([]instance.RepoRef{{Provider: "github", Owner: "other", Name: "other", Token: instance.TokenRef{Env: "UNRELATED_REPO_PAT"}}}, cfg.Repos...)
	project := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: selected.Owner, Project: selected.Project, Name: selected.Name}
	repo := terminalRepositoryRefForProject(cfg, project)
	fixture := &adoTerminalLabelsFixture{}
	server := fixture.server(t, repo)
	pointTerminalADOLabelerAt(t, server.URL)
	label, err := buildTerminalRunAbortLabeler(cfg, project, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := label(context.Background(), providers.UpdateWorkItemRequest{Repository: repo, ID: "42", AddLabels: []string{abortedRunLabel}}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:selected-repo-pat"))
	if fixture.posts != 1 {
		t.Fatalf("posts=%d", fixture.posts)
	}
	for _, got := range fixture.headers {
		if got != want {
			t.Fatal("selected ADO repository used another repository's token")
		}
	}
}

func TestADOTerminalLabelFactoryPreservesBearerScheme(t *testing.T) {
	cfg := adoTerminalConfig(instance.ADOAuthAzureCLI)
	repo := terminalRepositoryRefForProject(cfg, apiv1.RepoRef{})
	fixture := &adoTerminalLabelsFixture{}
	server := fixture.server(t, repo)
	pointTerminalADOLabelerAt(t, server.URL)
	updater, err := newTerminalRunAbortLabelProviderForProject(cfg, apiv1.RepoRef{}, staticTerminalTokenSource("entra-token"), journal.NewRegistryScrubber(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updater.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{Repository: repo, ID: "42", AddLabels: []string{abortedRunLabel}}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, got := range fixture.headers {
		if got != "Bearer entra-token" {
			t.Fatal("Entra credential was not sent as Bearer")
		}
	}
}
