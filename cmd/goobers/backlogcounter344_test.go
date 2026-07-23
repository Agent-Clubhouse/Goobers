package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
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
		if c := buildBacklogCounter(&instance.Config{}, wf, repoRef, nil, nil, ""); c != nil {
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
		if c := buildBacklogCounter(cfg, wf, repoRef, nil, nil, ""); c != nil {
			t.Fatalf("expected nil for a schedule-only workflow, got %+v", c)
		}
	})

	t.Run("wired with the target repo and selector labels", func(t *testing.T) {
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"goobers:ready": "true"}}},
		}}
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BACKLOG_TOK"}})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		c := buildBacklogCounter(cfg, wf, repoRef, resolver, &backlogTestRegistrar{}, "/instance/scheduler")
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
		if len(bc.labels) != 1 || bc.labels[0] != "goobers:ready" {
			t.Fatalf("labels = %v, want [goobers:ready] (the selector's keys)", bc.labels)
		}
		if bc.schedulerDir != "/instance/scheduler" {
			t.Fatalf("schedulerDir = %q, want /instance/scheduler", bc.schedulerDir)
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
