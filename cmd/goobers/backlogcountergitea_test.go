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
			// Before the fix this returned cfg.Repos[0]'s provider (github) —
			// the FIRST repo's kind, not the one the counted repoRef actually
			// names — misrouting this gaggle's backlog count to api.github.com.
			name: "multi-repo instance matches the target repo, not repos[0]",
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

// TestBacklogCounterGiteaCachesConfigLoad proves the Gitea arm caches its
// resolved forge BaseURL across ticks instead of re-reading and
// re-validating instance.yaml (giteaRepoRefForStage -> instance.LoadConfig)
// on every EligibleCount call, the way EligibleCount is actually driven (once
// per scheduler poll interval, for a long-lived daemon). Deleting the config
// file after the first successful call and asserting the second call STILL
// succeeds is the only way to observe this from the outside: without the
// cache, the second call re-reads the now-missing file and fails.
func TestBacklogCounterGiteaCachesConfigLoad(t *testing.T) {
	t.Setenv("GITEA_BACKLOG_TOK", "gitea-backlog-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":11,"title":"a","state":"open","labels":[{"name":"goobers:ready"}]}]`))
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

	if _, err := c.EligibleCount(context.Background()); err != nil {
		t.Fatalf("first EligibleCount: %v", err)
	}

	if err := os.Remove(instance.NewLayout(root).ConfigFile()); err != nil {
		t.Fatalf("remove instance config: %v", err)
	}

	if _, err := c.EligibleCount(context.Background()); err != nil {
		t.Fatalf("second EligibleCount after the config file was removed: %v — the Gitea arm re-read instance.yaml instead of using its cached BaseURL", err)
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

// TestBuildOpenPRRefresherChecksPerGaggleRepoProvider is the multi-repo analog
// of TestBuildOpenPRRefresherRefusesNonGitHubRepo: both the refusal and the
// hardcoded-GitHub repoRef bug were keyed off cfg.Repos[0] instead of each
// capped gaggle's own resolved repo (via configuredRepoForProject).
func TestBuildOpenPRRefresherChecksPerGaggleRepoProvider(t *testing.T) {
	t.Run("repos[0] gitea does not mask a github gaggle", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "https://gitea.example.test"},
			{Provider: "github", Owner: "acme", Name: "web"},
		}}
		workflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{
			Gaggle: "main", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 1},
		}}}
		projects := map[string]apiv1.RepoRef{
			"main": {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		}
		set, err := buildOpenPRRefresher(cfg, workflows, projects, &backlogTestRegistrar{}, nil, t.TempDir(), nil)
		if err != nil {
			t.Fatalf("a github-projected gaggle must wire cleanly even when cfg.Repos[0] is gitea: %v", err)
		}
		if set == nil {
			t.Fatal("expected a non-nil refresher set")
		}
	})

	t.Run("repos[0] github does not mask a gitea gaggle", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web"},
			{Provider: "gitea", Owner: "acme", Name: "widgets", BaseURL: "https://gitea.example.test"},
		}}
		workflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{
			Gaggle: "site", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 1},
		}}}
		projects := map[string]apiv1.RepoRef{
			"site": {Provider: apiv1.ProviderGitea, Owner: "acme", Name: "widgets"},
		}
		_, err := buildOpenPRRefresher(cfg, workflows, projects, &backlogTestRegistrar{}, nil, t.TempDir(), nil)
		if err == nil {
			t.Fatal("expected the gitea-projected gaggle to be refused, not silently pointed at github")
		}
		if !strings.Contains(err.Error(), "maxOpenPRs") || !strings.Contains(err.Error(), "gitea") {
			t.Fatalf("error = %v, want an explicit unsupported-provider message naming gitea", err)
		}
	})
}
