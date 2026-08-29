package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		c, err := buildBacklogCounter(&instance.Config{}, apiv1.Gaggle{}, wf, repoRef, nil, nil, "", nil, "")
		if err != nil {
			t.Fatalf("buildBacklogCounter: %v", err)
		}
		if c != nil {
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
		c, err := buildBacklogCounter(cfg, apiv1.Gaggle{}, wf, repoRef, nil, nil, "", nil, "")
		if err != nil {
			t.Fatalf("buildBacklogCounter: %v", err)
		}
		if c != nil {
			t.Fatalf("expected nil for a schedule-only workflow, got %+v", c)
		}
	})

	t.Run("wired with the target repo and selector labels", func(t *testing.T) {
		gaggle := apiv1.Gaggle{Spec: apiv1.GaggleSpec{Backlog: apiv1.BacklogRef{
			FieldPredicate: `fields["state"] == "open"`,
		}}}
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{
				Type: apiv1.TriggerBacklogItem,
				Selector: map[string]string{
					"goobers:ready":    "true",
					"goobers:approved": "true",
				},
				LabelPredicate: `("size:s" in labels || "size:m" in labels) && !("platform:windows" in labels)`,
				FieldPredicate: `fields["number"] >= 10`,
			}},
		}}
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		quota := localscheduler.NewProviderQuotaState()
		c, err := buildBacklogCounter(cfg, gaggle, wf, repoRef, resolver, &backlogTestRegistrar{}, "/instance/scheduler", quota, "")
		if err != nil {
			t.Fatalf("buildBacklogCounter: %v", err)
		}
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
		matched, err := bc.labelPredicate.Matches([]string{"goobers:ready", "goobers:approved", "size:m"})
		if err != nil || !matched {
			t.Fatalf("compiled predicate match = %v, err = %v, want true", matched, err)
		}
		matched, err = bc.fieldPredicate.Matches(fieldpredicate.Fields{"state": "open", "number": int64(10)})
		if err != nil || !matched {
			t.Fatalf("compiled field predicate match = %v, err = %v, want true", matched, err)
		}
		for _, fields := range []fieldpredicate.Fields{
			{"state": "closed", "number": int64(10)},
			{"state": "open", "number": int64(9)},
		} {
			matched, err = bc.fieldPredicate.Matches(fields)
			if err != nil || matched {
				t.Fatalf("compiled field predicate match for %v = %v, err = %v, want false", fields, matched, err)
			}
		}
	})

	t.Run("token ref follows the workflow's own repo", func(t *testing.T) {
		multi := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BACKLOG_TOK"}},
			{Provider: "github", Owner: "masra", Name: "site", Token: instance.TokenRef{Env: "BACKLOG_TOK_B"}},
		}}
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"goobers:ready": "true"}}},
		}}
		siteRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "masra", Name: "site"}
		c, err := buildBacklogCounter(multi, apiv1.Gaggle{}, wf, siteRef, nil, nil, "", nil, "")
		if err != nil {
			t.Fatalf("buildBacklogCounter: %v", err)
		}
		bc, ok := c.(*backlogCounter)
		if !ok {
			t.Fatalf("counter type = %T, want *backlogCounter", c)
		}
		// #2692 sibling: the counter queries the workflow's repo, so it must
		// authenticate with that repo's ref — not the first repo's.
		if bc.ref != "masra/site" {
			t.Fatalf("credential ref = %q, want the workflow repo's own masra/site", bc.ref)
		}
		if bc.repo.Owner != "masra" || bc.repo.Name != "site" {
			t.Fatalf("repo = %+v, want masra/site", bc.repo)
		}
	})

	t.Run("desired refill derives schedule workflow backlog eligibility", func(t *testing.T) {
		gaggle := apiv1.Gaggle{Spec: apiv1.GaggleSpec{RequireLabels: []string{"gaggle-default"}}}
		wf := &apiv1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
			Spec: apiv1.WorkflowSpec{
				Gaggle:    "goobers",
				Triggers:  []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@every 1h"}},
				Readiness: apiv1.ReadinessConditions{DesiredConcurrentRuns: 2, MaxConcurrentRuns: 4},
				Start:     "query-backlog",
				Tasks: []apiv1.Task{{
					Name: "query-backlog",
					Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
					Inputs: map[string]string{
						"trustLabel":      "goobers:approved",
						"requireLabels":   "goobers:ready",
						"excludeLabels":   "goobers/status:in-review",
						"respectAssignee": "true",
					},
				}},
			},
		}
		counter, err := buildRefillDemandCounter(
			cfg, gaggle, wf, repoRef, nil, nil, "/instance/scheduler", "goobersbot", nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		refill, ok := counter.(*backlogCounter)
		if !ok {
			t.Fatalf("counter type = %T, want *backlogCounter", counter)
		}
		if got, want := refill.labels, []string{"goobers:approved", "goobers:ready"}; !slices.Equal(got, want) {
			t.Fatalf("labels = %v, want %v", got, want)
		}
		if !refill.respectAssignee || refill.assignedTo != "goobersbot" {
			t.Fatalf("assignee scope = enabled:%v value:%q, want goobersbot", refill.respectAssignee, refill.assignedTo)
		}
		matched, err := refill.labelPredicate.Matches([]string{"goobers:approved", "goobers:ready"})
		if err != nil || !matched {
			t.Fatalf("eligible labels match = %v, err = %v, want true", matched, err)
		}
		matched, err = refill.labelPredicate.Matches([]string{"goobers:approved", "goobers:ready", providers.LabelClaimed})
		if err != nil || matched {
			t.Fatalf("claimed labels match = %v, err = %v, want false", matched, err)
		}
	})

	t.Run("pr remediation uses claim-aware pull request demand", func(t *testing.T) {
		scheduleCfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "other", Token: instance.TokenRef{Env: "OTHER_TOK"}},
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BACKLOG_TOK"}},
		}}
		wf := &apiv1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "pr-remediation"},
			Spec: apiv1.WorkflowSpec{
				Gaggle:   "goobers",
				Start:    "select",
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@every 1h", Priority: 100}},
				Tasks: []apiv1.Task{{
					Name: "select",
					Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "update-behind-pr"}},
				}},
			},
		}
		counter := buildScheduleDemandCounter(
			scheduleCfg, wf, repoRef, nil, nil, "/instance/scheduler", "acme", nil,
		)
		remediation, ok := counter.(*remediationDemandCounter)
		if !ok {
			t.Fatalf("counter type = %T, want *remediationDemandCounter", counter)
		}
		if remediation.ref != "acme/web" {
			t.Fatalf("credential ref = %q, want target repository acme/web", remediation.ref)
		}
		if remediation.headPrefix != "acme/" || remediation.gaggle != "goobers" {
			t.Fatalf("remediation counter scope = prefix %q gaggle %q", remediation.headPrefix, remediation.gaggle)
		}
	})

	t.Run("invalid predicate fails closed", func(t *testing.T) {
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{
				Type:           apiv1.TriggerBacklogItem,
				LabelPredicate: `labels.size() > 0`,
			}},
		}}
		if _, err := buildBacklogCounter(cfg, apiv1.Gaggle{}, wf, repoRef, nil, nil, "", nil, ""); err == nil {
			t.Fatal("buildBacklogCounter succeeded with an unsupported predicate")
		}
	})

	t.Run("invalid field predicate fails closed", func(t *testing.T) {
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{
				Type:           apiv1.TriggerBacklogItem,
				FieldPredicate: `fields.number == 1`,
			}},
		}}
		if _, err := buildBacklogCounter(cfg, apiv1.Gaggle{}, wf, repoRef, nil, nil, "", nil, ""); err == nil {
			t.Fatal("buildBacklogCounter succeeded with an unsupported field predicate")
		}
	})
}

func TestBacklogCounterAppliesExactLabelPredicate(t *testing.T) {
	t.Setenv("BACKLOG_TOK", "backlog-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	predicate, err := labelpredicate.Compile(
		`("size:s" in labels || "size:m" in labels) && !("platform:windows" in labels)`,
		[]string{"area:runner"},
		[]string{providers.LabelClaimed},
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	server := newFakeGitHubServer(t, "acme", "web")
	server.addIssue(1, "Small runner item", "area:runner", "size:s")
	server.addIssue(2, "Windows medium item", "area:runner", "size:m", "platform:windows")
	server.addIssue(3, "Large runner item", "area:runner", "size:l")
	server.addIssue(4, "Small docs item", "area:docs", "size:s")
	server.addIssue(5, "Claimed runner item", "area:runner", "size:s", providers.LabelClaimed)
	server.addIssue(6, "Other owner runner item", "area:runner", "size:s")
	setFakeIssueAssignee(server, 1, "goobersbot")
	setFakeIssueAssignee(server, 5, "goobersbot")
	setFakeIssueAssignee(server, 6, "someone-else")
	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	counter := &backlogCounter{
		ref:             "acme/web",
		repo:            providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		labels:          predicate.RequiredLabels(),
		labelPredicate:  predicate,
		respectAssignee: true,
		assignedTo:      "goobersbot",
		resolver:        resolver,
		reg:             &backlogTestRegistrar{},
	}
	count, err := counter.EligibleCount(context.Background())
	if err != nil {
		t.Fatalf("EligibleCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want only the non-Windows small runner item", count)
	}
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

func TestBacklogCounterAdvancesBoundedPagesAndTracksProviderQuota(t *testing.T) {
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
			if page != "1" {
				t.Fatalf("first request page = %q, want 1", page)
			}
			w.Header().Set("X-RateLimit-Remaining", "9")
			_, _ = fmt.Fprint(w, "[")
			for i := 1; i <= 100; i++ {
				if i > 1 {
					_, _ = fmt.Fprint(w, ",")
				}
				_, _ = fmt.Fprintf(w, `{"number":%d,"title":"other","state":"open"}`, i)
			}
			_, _ = fmt.Fprint(w, "]")
		case 2:
			if page != "2" {
				t.Fatalf("second request page = %q, want 2", page)
			}
			w.Header().Set("X-RateLimit-Remaining", "8")
			_, _ = w.Write([]byte(`[{"number":101,"title":"wanted issue","state":"open","labels":[{"name":"wanted"}]}]`))
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
	firstAdmission := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	if firstAdmission.RemainingBefore != 10 || firstAdmission.RemainingAfter != 9 {
		t.Fatalf("admission budget = %+v, want 10 remaining before and 9 after", firstAdmission)
	}
	predicate, err := labelpredicate.Compile(`"wanted" in labels`, nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	counter := &backlogCounter{
		ref:            "acme/web",
		repo:           providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		labelPredicate: predicate,
		resolver:       resolver,
		reg:            &backlogTestRegistrar{},
		quota:          quota,
	}
	count, err := counter.EligibleCount(localscheduler.WithProviderPollBudget(context.Background(), firstAdmission))
	if err != nil {
		t.Fatalf("EligibleCount page 1: %v", err)
	}
	if count != 0 || requests != 1 {
		t.Fatalf("count=%d requests=%d, want one bounded nonmatching page", count, requests)
	}
	secondAdmission := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	count, err = counter.EligibleCount(localscheduler.WithProviderPollBudget(context.Background(), secondAdmission))
	if err != nil {
		t.Fatalf("EligibleCount page 2: %v", err)
	}
	if count != 1 || requests != 2 {
		t.Fatalf("count=%d requests=%d, want matching issue from the next bounded page", count, requests)
	}
	next := quota.ReservePolls(apiv1.ProviderGitHub, now, 1)
	if next.RemainingBefore != 8 {
		t.Fatalf("remaining quota before next poll = %d, want 8 after both bounded requests", next.RemainingBefore)
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

func TestBacklogCounterUsesOneBoundedPageWithinBudget(t *testing.T) {
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
	count, err := counter.EligibleCount(ctx)
	if err != nil {
		t.Fatalf("EligibleCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want one item from the bounded page", count)
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
