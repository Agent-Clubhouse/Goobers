package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

type backlogTestRegistrar struct{ registered [][]byte }

func (r *backlogTestRegistrar) Register(secret []byte) {
	r.registered = append(r.registered, append([]byte(nil), secret...))
}

// TestBuildBacklogCounter is #344's composition-root wiring test, mirroring
// TestBuildEscalationNotifier: nil for a repo-less instance or a workflow
// with no backlog-item trigger; wired with the target repo and the
// trigger's selector keys as required labels otherwise.
func TestBuildBacklogCounter(t *testing.T) {
	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}

	t.Run("nil for a repo-less instance", func(t *testing.T) {
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"goobers": "true"}}},
		}}
		if c := buildBacklogCounter(&instance.Config{}, wf, repoRef, nil, nil, "", nil); c != nil {
			t.Fatalf("expected nil for no repos, got %+v", c)
		}
	})

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BACKLOG_TOK"}},
	}}

	t.Run("nil for a workflow with no backlog-item trigger", func(t *testing.T) {
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@every 1h"}},
		}}
		if c := buildBacklogCounter(cfg, wf, repoRef, nil, nil, "", nil); c != nil {
			t.Fatalf("expected nil for a schedule-only workflow, got %+v", c)
		}
	})

	t.Run("wired with the target repo and selector labels", func(t *testing.T) {
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{
				"goobers:ready":    "true",
				"goobers:approved": "true",
			}}},
		}}
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		quota := localscheduler.NewProviderQuotaState()
		c := buildBacklogCounter(cfg, wf, repoRef, resolver, &backlogTestRegistrar{}, "/instance/scheduler", quota)
		if c == nil {
			t.Fatal("expected a non-nil counter for a backlog-item-triggered, repo-backed workflow")
		}
		bc, ok := c.(*backlogCounter)
		if !ok {
			t.Fatalf("counter type = %T, want *backlogCounter", c)
		}
		if bc.repo.Owner != "acme" || bc.repo.Name != "web" {
			t.Fatalf("repo = %+v, want acme/web", bc.repo)
		}
		if got, want := bc.labels, []string{"goobers:approved", "goobers:ready"}; !slices.Equal(got, want) {
			t.Fatalf("labels = %v, want canonical order %v", got, want)
		}
		if bc.schedulerDir != "/instance/scheduler" {
			t.Fatalf("schedulerDir = %q, want /instance/scheduler", bc.schedulerDir)
		}
		if bc.quota == nil {
			t.Fatal("provider quota observer was not wired")
		}
	})
}

// TestBacklogCounterResolvesTokenPerCallAndQueriesProvider mirrors
// TestEscalationCommenterResolvesTokenPerCall: the counter resolves its
// token fresh on each EligibleCount call (not captured at construction),
// registers it for scrubbing, and queries the provider with the selector's
// labels — proving #344's fan-out counting actually reaches a real
// ListWorkItems call, not just returning a hardcoded value.
func TestBacklogCounterResolvesTokenPerCallAndQueriesProvider(t *testing.T) {
	t.Setenv("BACKLOG_TOK", "backlog-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	reg := &backlogTestRegistrar{}

	server := newFakeGitHubServer(t, "acme", "web")
	server.addIssue(1, "Item 1", "goobers:ready")
	server.addIssue(2, "Item 2", "goobers:ready")
	server.addIssue(3, "Item 3") // missing the required label

	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	bc := &backlogCounter{
		ref:      "acme/web",
		repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		labels:   []string{"goobers:ready"},
		resolver: resolver,
		reg:      reg,
	}

	count, err := bc.EligibleCount(context.Background())
	if err != nil {
		t.Fatalf("EligibleCount: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (only the labeled items)", count)
	}
	if len(reg.registered) == 0 || string(reg.registered[0]) != "backlog-token-value" {
		t.Fatalf("registered secrets = %v, want the resolved token registered for scrubbing", reg.registered)
	}
}

func TestBacklogCounterPaginatesAndTracksProviderQuota(t *testing.T) {
	t.Setenv("BACKLOG_TOK", "backlog-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		page := r.URL.Query().Get("page")
		if r.URL.Query().Get("per_page") != "100" {
			t.Fatalf("query = %q, want 100-item pages", r.URL.RawQuery)
		}
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(resetAt.Unix()))
		switch requests {
		case 1:
			if page != "" {
				t.Fatalf("first request page = %q, want provider default", page)
			}
			w.Header().Set("X-RateLimit-Remaining", "9")
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2&per_page=100>; rel="next"`, r.Host, r.URL.Path))
			_, _ = w.Write([]byte(`[{"number":1,"title":"pull request","state":"open","pull_request":{"url":"pr-url"}}]`))
		case 2:
			if page != "2" {
				t.Fatalf("second request page = %q, want 2", page)
			}
			w.Header().Set("X-RateLimit-Remaining", "8")
			_, _ = w.Write([]byte(`[{"number":2,"title":"ready issue","state":"open"}]`))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = server.URL
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 10, resetAt)
	admission := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	if admission.RemainingBefore != 10 || admission.RemainingAfter != 9 {
		t.Fatalf("admission budget = %+v, want 10 remaining before and 9 after", admission)
	}
	counter := &backlogCounter{
		ref:      "acme/web",
		repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		resolver: resolver,
		reg:      &backlogTestRegistrar{},
		quota:    quota,
	}
	ctx := localscheduler.WithProviderPollBudget(context.Background(), admission)
	count, err := counter.EligibleCount(ctx)
	if err != nil {
		t.Fatalf("EligibleCount: %v", err)
	}
	if count != 1 || requests != 2 {
		t.Fatalf("count=%d requests=%d, want the page-2 issue from two provider requests", count, requests)
	}
	next := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	if next.RemainingBefore != 8 {
		t.Fatalf("remaining quota before next poll = %d, want 8 after both paginated requests", next.RemainingBefore)
	}
}

func TestBacklogCounterRetriesTransientFailureWithinQuota(t *testing.T) {
	t.Setenv("BACKLOG_TOK", "backlog-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, "temporary provider failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "1")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(resetAt.Unix()))
		_, _ = w.Write([]byte(`[{"number":1,"title":"ready issue","state":"open"}]`))
	}))
	defer server.Close()

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = server.URL
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 3, resetAt)
	admission := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	counter := &backlogCounter{
		ref:      "acme/web",
		repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		resolver: resolver,
		reg:      &backlogTestRegistrar{},
		quota:    quota,
	}
	ctx := localscheduler.WithProviderPollBudget(context.Background(), admission)
	count, err := counter.EligibleCount(ctx)
	if err != nil {
		t.Fatalf("EligibleCount: %v", err)
	}
	if count != 1 || requests != 2 {
		t.Fatalf("count=%d requests=%d, want success after one transient retry", count, requests)
	}
	next := quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 1)
	if next.RemainingBefore != 1 {
		t.Fatalf("remaining quota before next poll = %d, want both attempts charged", next.RemainingBefore)
	}
}

func TestBacklogCounterStopsPaginationAtBudget(t *testing.T) {
	t.Setenv("BACKLOG_TOK", "backlog-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(resetAt.Unix()))
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2&per_page=100>; rel="next"`, r.Host, r.URL.Path))
		_, _ = w.Write([]byte(`[{"number":1,"title":"ready issue","state":"open"}]`))
	}))
	defer server.Close()

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = server.URL
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 1, resetAt)
	admission := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	counter := &backlogCounter{
		ref:      "acme/web",
		repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		resolver: resolver,
		reg:      &backlogTestRegistrar{},
		quota:    quota,
	}
	ctx := localscheduler.WithProviderPollBudget(context.Background(), admission)
	_, err = counter.EligibleCount(ctx)
	var budgetErr *localscheduler.ProviderPollBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("EligibleCount error = %v, want provider polling budget exhaustion", err)
	}
	if requests != 1 {
		t.Fatalf("provider requests = %d, want pagination stopped after the admitted page", requests)
	}
}

func TestBacklogCounterSnapshotHitRefundsQuota(t *testing.T) {
	t.Setenv("BACKLOG_TOK", "backlog-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("X-RateLimit-Remaining", "1")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(resetAt.Unix()))
		_, _ = w.Write([]byte(`[{"number":1,"title":"ready issue","state":"open"}]`))
	}))
	defer server.Close()

	prev := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = server.URL
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = prev })

	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 2, resetAt)
	counter := &backlogCounter{
		ref:          "acme/web",
		repo:         providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		resolver:     resolver,
		reg:          &backlogTestRegistrar{},
		schedulerDir: t.TempDir(),
		quota:        quota,
	}
	ctx := providersnapshot.WithID(context.Background(), "shared-tick")
	for i := 0; i < 2; i++ {
		decision := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
		if decision.Allowed != 1 {
			t.Fatalf("poll %d reservation = %+v, want admitted", i+1, decision)
		}
		pollCtx := localscheduler.WithProviderPollBudget(ctx, decision)
		if count, err := counter.EligibleCount(pollCtx); err != nil || count != 1 {
			t.Fatalf("poll %d count=%d err=%v, want one cached item", i+1, count, err)
		}
	}
	next := quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 1)
	if next.RemainingBefore != 1 || next.Allowed != 1 {
		t.Fatalf("budget after snapshot hit = %+v, want cached reservation refunded", next)
	}
	if count, err := counter.EligibleCount(ctx); err != nil || count != 1 {
		t.Fatalf("zero-budget snapshot count=%d err=%v, want one cached item", count, err)
	}
	if requests != 1 {
		t.Fatalf("provider requests = %d, want second poll served by shared snapshot", requests)
	}
}

func TestBacklogCounterRefundsPreRequestFailure(t *testing.T) {
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "MISSING_BACKLOG_TOKEN"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	now := time.Now()
	resetAt := now.Add(time.Hour)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 1, resetAt)
	admission := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	counter := &backlogCounter{
		ref:      "acme/web",
		repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		resolver: resolver,
		reg:      &backlogTestRegistrar{},
		quota:    quota,
	}

	ctx := localscheduler.WithProviderPollBudget(context.Background(), admission)
	if _, err := counter.EligibleCount(ctx); err == nil {
		t.Fatal("EligibleCount succeeded without a configured token")
	}
	next := quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 1)
	if next.RemainingBefore != 1 || next.Allowed != 1 {
		t.Fatalf("budget after pre-request failure = %+v, want reservation refunded", next)
	}
}
