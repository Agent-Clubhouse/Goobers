package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// giteaCounterInstance writes a minimal Gitea instance config to a temp root so
// newGiteaProviderForStage can resolve the forge BaseURL the way it does in a
// real stage.
func giteaCounterInstance(t *testing.T, baseURL string) (string, *instance.Config) {
	t.Helper()
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(filepath.Dir(l.ConfigFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "gitea",
		Owner:    "acme",
		Name:     "widgets",
		BaseURL:  baseURL,
		Token:    instance.TokenRef{Env: "GITEA_BACKLOG_TOK"},
	}}}
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

// TestBacklogCounterRepoRefCarriesConfiguredProvider pins the counted repo's
// kind to the instance's own declared provider. The kind is what
// newCounterProvider dispatches on, so an unconditional GitHub here sent a
// Gitea instance's backlog count to api.github.com.
func TestBacklogCounterRepoRefCarriesConfiguredProvider(t *testing.T) {
	repoRef := apiv1.RepoRef{Owner: "acme", Name: "widgets"}
	tests := []struct {
		name string
		cfg  *instance.Config
		want providers.ProviderKind
	}{
		{
			name: "gitea instance counts against gitea",
			cfg:  &instance.Config{Repos: []instance.RepoRef{{Provider: "gitea", Owner: "acme", Name: "widgets"}}},
			want: providers.ProviderGitea,
		},
		{
			name: "github instance stays github",
			cfg:  &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web"}}},
			want: providers.ProviderGitHub,
		},
		{
			name: "unset provider defaults to github",
			cfg:  &instance.Config{Repos: []instance.RepoRef{{Owner: "acme", Name: "web"}}},
			want: providers.ProviderGitHub,
		},
		{
			name: "mixed instance selects the matching non-first repo",
			cfg: &instance.Config{Repos: []instance.RepoRef{
				{Provider: "github", Owner: "acme", Name: "web"},
				{Provider: "gitea", Owner: "acme", Name: "widgets"},
			}},
			want: providers.ProviderGitea,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := backlogCounterRepoRef(tc.cfg, repoRef).Provider; got != tc.want {
				t.Fatalf("provider = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildOpenPRRefresherValidatesTheSelectedRepoProvider(t *testing.T) {
	workflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{
		Gaggle: "selected", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 3},
	}}}

	t.Run("non-first gitea binding is refused", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN"}},
			{Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "https://gitea.example.test", Token: instance.TokenRef{Env: "GITEA_TOKEN"}},
		}}
		projects := map[string]apiv1.RepoRef{
			"selected": {Provider: apiv1.ProviderGitea, Owner: "acme", Name: "widgets"},
		}
		_, err := buildOpenPRRefresher(cfg, workflows, projects, &backlogTestRegistrar{}, nil, t.TempDir(), nil)
		if err == nil || !strings.Contains(err.Error(), "gitea") {
			t.Fatalf("error = %v, want selected gitea binding refused", err)
		}
	})

	t.Run("non-first github binding is accepted", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "https://gitea.example.test", Token: instance.TokenRef{Env: "GITEA_TOKEN"}},
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN"}},
		}}
		projects := map[string]apiv1.RepoRef{
			"selected": {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		}
		set, err := buildOpenPRRefresher(cfg, workflows, projects, &backlogTestRegistrar{}, nil, t.TempDir(), nil)
		if err != nil || set == nil {
			t.Fatalf("set = %v, error = %v; want selected github binding accepted", set, err)
		}
	})
}

// TestBacklogCounterCountsAgainstGitea is the end-to-end proof that a Gitea
// instance's backlog fan-out count reaches the configured self-hosted forge
// with the Gitea token, never api.github.com. Routing to the wrong provider
// prevents the scheduler from observing eligible work.
func TestBacklogCounterCountsAgainstGitea(t *testing.T) {
	t.Setenv("GITEA_BACKLOG_TOK", "gitea-backlog-token")

	var mu sync.Mutex
	var paths []string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Two open items, both carrying the selector labels.
		_, _ = w.Write([]byte(`[
			{"number":11,"title":"a","state":"open","labels":[{"name":"goobers:ready"}]},
			{"number":12,"title":"b","state":"open","labels":[{"name":"goobers:ready"}]}
		]`))
	}))
	t.Cleanup(srv.Close)

	root, cfg := giteaCounterInstance(t, srv.URL)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/widgets", Env: "GITEA_BACKLOG_TOK"}})
	if err != nil {
		t.Fatal(err)
	}
	wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Triggers: []apiv1.Trigger{{
			Type:     apiv1.TriggerBacklogItem,
			Selector: map[string]string{"goobers:ready": "true"},
		}},
	}}
	c, err := buildBacklogCounter(cfg, apiv1.Gaggle{}, wf,
		apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		resolver, &backlogTestRegistrar{}, filepath.Join(root, "scheduler"), nil, root)
	if err != nil {
		t.Fatalf("buildBacklogCounter: %v", err)
	}
	if c == nil {
		t.Fatal("expected a counter")
	}

	count, err := c.EligibleCount(context.Background())
	if err != nil {
		t.Fatalf("EligibleCount against gitea: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("no request reached the gitea test server -- the counter did not route to gitea")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/api/v1/") {
			t.Fatalf("request path %q is not a gitea API route", p)
		}
	}
	for _, auth := range auths {
		if auth != "token gitea-backlog-token" {
			t.Fatalf("Authorization = %q, want the gitea token scheme", auth)
		}
	}
}

func TestBacklogCounterGiteaCachesStaticBaseURL(t *testing.T) {
	t.Setenv("GITEA_BACKLOG_TOK", "gitea-backlog-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":11,"title":"a","state":"open","labels":[{"name":"goobers:ready"}]}]`))
	}))
	t.Cleanup(srv.Close)

	root, cfg := giteaCounterInstance(t, srv.URL)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/widgets", Env: "GITEA_BACKLOG_TOK"}})
	if err != nil {
		t.Fatal(err)
	}
	wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{Triggers: []apiv1.Trigger{{
		Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"goobers:ready": "true"},
	}}}}
	counter, err := buildBacklogCounter(cfg, apiv1.Gaggle{}, wf,
		apiv1.RepoRef{Owner: "acme", Name: "widgets"}, resolver,
		&backlogTestRegistrar{}, filepath.Join(root, "scheduler"), nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := counter.EligibleCount(context.Background()); err != nil {
		t.Fatalf("first EligibleCount: %v", err)
	}
	if err := os.Remove(instance.NewLayout(root).ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := counter.EligibleCount(context.Background()); err != nil {
		t.Fatalf("second EligibleCount with cached base URL: %v", err)
	}
}

// TestBuildOpenPRRefresherRefusesNonGitHubRepo covers the one audited surface
// that CANNOT be routed: the #353 open-PR cap polls ListOpenPullRequests, which
// only the GitHub backend implements. Building a GitHub client for a Gitea repo
// would 401 on every refresh and leave the cap silently reading a stale count,
// so the unsupported combination must fail loudly at wiring time instead.
func TestBuildOpenPRRefresherRefusesNonGitHubRepo(t *testing.T) {
	cappedWorkflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{
		Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 3},
	}}}

	t.Run("gitea repo is refused", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{{
			Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "https://gitea.example.test",
		}}}
		_, err := buildOpenPRRefresher(cfg, cappedWorkflows, nil, &backlogTestRegistrar{}, nil, t.TempDir(), nil)
		if err == nil {
			t.Fatal("expected maxOpenPRs on a gitea repo to be refused, not silently pointed at github")
		}
		if !strings.Contains(err.Error(), "maxOpenPRs") || !strings.Contains(err.Error(), "gitea") {
			t.Fatalf("error = %v, want an explicit unsupported-provider message", err)
		}
	})

	t.Run("uncapped gitea instance builds nothing", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{{
			Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "https://gitea.example.test",
		}}}
		uncapped := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{}}}
		refresher, err := buildOpenPRRefresher(cfg, uncapped, nil, &backlogTestRegistrar{}, nil, t.TempDir(), nil)
		if err != nil {
			t.Fatalf("an uncapped gitea instance must wire cleanly: %v", err)
		}
		if refresher != nil {
			t.Fatal("expected no refresher when no workflow opts into the cap")
		}
	})
}
