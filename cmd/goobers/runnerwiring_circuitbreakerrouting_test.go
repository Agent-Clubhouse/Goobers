package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// circuitBreakerRepoRecorder records the repository every breaker provider call
// is routed to, across all three Commenter surfaces: the escalated/aborted path
// mutates work items, the completed path lists and updates comments.
type circuitBreakerRepoRecorder struct {
	repos []providers.RepositoryRef
}

func (r *circuitBreakerRepoRecorder) ListComments(_ context.Context, repository providers.RepositoryRef, _ string) ([]providers.Comment, error) {
	r.repos = append(r.repos, repository)
	return nil, nil
}

func (r *circuitBreakerRepoRecorder) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	r.repos = append(r.repos, req.Repository)
	return providers.WorkItem{}, nil
}

func (r *circuitBreakerRepoRecorder) UpdateComment(_ context.Context, repository providers.RepositoryRef, _, _ string) error {
	r.repos = append(r.repos, repository)
	return nil
}

func circuitBreakerRoutingRunnerConfig(t *testing.T, cfg *instance.Config, project apiv1.RepoRef, runID, itemID string) (instance.Layout, *circuitBreakerRepoRecorder, func(journal.RunPhase) error) {
	t.Helper()

	recorder := &circuitBreakerRepoRecorder{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return recorder }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	runnerCfg, _, err := buildRunnerConfig(runnerCompositionInput{
		Layout:         l,
		Config:         cfg,
		SharedRegistry: journal.NewRegistryScrubber(),
		GaggleProject:  project,
		SandboxPosture: instance.SandboxDisabled,
	})
	if err != nil {
		t.Fatalf("buildRunnerConfig: %v", err)
	}
	if runnerCfg.NotifyTerminal == nil {
		t.Fatal("expected a wired terminal notifier")
	}
	return l, recorder, func(phase journal.RunPhase) error {
		return runnerCfg.NotifyTerminal(runID, phase, "open-pr-gate")
	}
}

func assertCircuitBreakerRoutedTo(t *testing.T, recorder *circuitBreakerRepoRecorder, want providers.RepositoryRef) {
	t.Helper()
	if len(recorder.repos) == 0 {
		t.Fatal("circuit breaker made no provider call to route")
	}
	for _, got := range recorder.repos {
		if got != want {
			t.Fatalf("breaker routed to %+v, want %+v", got, want)
		}
	}
}

// TestTerminalCircuitBreakerRoutesToGaggleRepository covers #4243: on a
// multi-repo instance the failure-streak comment, park, and reset must land on
// the repository the run's own gaggle names, not cfg.Repos[0] — the two repos'
// issue numbers collide, so the wrong route silently mutates an unrelated item.
func TestTerminalCircuitBreakerRoutesToGaggleRepository(t *testing.T) {
	t.Setenv("CB_ROUTE_TOK_WEB", "web-token-value")
	t.Setenv("CB_ROUTE_TOK_API", "api-token-value")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_WEB"}},
		{Provider: "github", Owner: "acme", Name: "api", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_API"}},
	}}
	second := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "api"}
	want := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}

	for _, phase := range []journal.RunPhase{journal.PhaseEscalated, journal.PhaseAborted, journal.PhaseCompleted} {
		t.Run(string(phase), func(t *testing.T) {
			_, recorder, notify := circuitBreakerRoutingRunnerConfig(t, cfg, second, "run-"+string(phase), "7")
			if err := notify(phase); err != nil {
				t.Fatalf("notify %s: %v", phase, err)
			}
			assertCircuitBreakerRoutedTo(t, recorder, want)
		})
	}
}

// TestTerminalCircuitBreakerSingleRepoUnchanged is the regression half of
// #4243: a single-repo instance keeps routing to its only repo, whether or not
// the gaggle declares a project.
func TestTerminalCircuitBreakerSingleRepoUnchanged(t *testing.T) {
	t.Setenv("CB_ROUTE_TOK_WEB", "web-token-value")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_WEB"}},
	}}
	want := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

	for name, project := range map[string]apiv1.RepoRef{
		"no-project": {},
		"project":    {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	} {
		t.Run(name, func(t *testing.T) {
			_, recorder, notify := circuitBreakerRoutingRunnerConfig(t, cfg, project, "run-single-"+name, "9")
			if err := notify(journal.PhaseEscalated); err != nil {
				t.Fatalf("notify: %v", err)
			}
			assertCircuitBreakerRoutedTo(t, recorder, want)
		})
	}
}
