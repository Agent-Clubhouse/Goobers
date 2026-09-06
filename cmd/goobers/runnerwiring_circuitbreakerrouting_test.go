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

// itemRepo is the repository recordItemRepository would have recorded for
// itemID at selection time (#4417): routing is now resolved per-item from
// this recording, not from project/the gaggle's static configuration, so
// callers pass whichever repo they want the item to have been "selected"
// against, independent of project (still threaded to buildRunnerConfig for
// its other, unrelated uses — credential resolution, workspace wiring).
func circuitBreakerRoutingRunnerConfig(t *testing.T, cfg *instance.Config, project apiv1.RepoRef, itemRepo providers.RepositoryRef, runID, itemID string) (instance.Layout, *circuitBreakerRepoRecorder, func(journal.RunPhase) error) {
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
	seedItemRepositoryForTest(t, l, runID, itemID, itemRepo)

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

// TestTerminalCircuitBreakerRoutesToItemRepository covers #4417 (superseding
// #4243's gaggle-level version): on a multi-repo instance the failure-streak
// comment, park, and reset must land on the repository the CLAIMED ITEM was
// actually selected against, not the gaggle's declared project and not
// cfg.Repos[0] — the two repos' issue numbers collide, so the wrong route
// silently mutates an unrelated item. The gaggle here declares NO project at
// all (the #4243 mechanism this supersedes would have fallen back to
// cfg.Repos[0], the wrong repo), proving routing depends only on the item's
// own recorded identity.
func TestTerminalCircuitBreakerRoutesToItemRepository(t *testing.T) {
	t.Setenv("CB_ROUTE_TOK_WEB", "web-token-value")
	t.Setenv("CB_ROUTE_TOK_API", "api-token-value")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_WEB"}},
		{Provider: "github", Owner: "acme", Name: "api", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_API"}},
	}}
	want := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}

	for _, phase := range []journal.RunPhase{journal.PhaseEscalated, journal.PhaseAborted, journal.PhaseCompleted} {
		t.Run(string(phase), func(t *testing.T) {
			_, recorder, notify := circuitBreakerRoutingRunnerConfig(t, cfg, apiv1.RepoRef{}, want, "run-"+string(phase), "7")
			if err := notify(phase); err != nil {
				t.Fatalf("notify %s: %v", phase, err)
			}
			assertCircuitBreakerRoutedTo(t, recorder, want)
		})
	}
}

// TestTerminalCircuitBreakerSingleRepoUnchanged is the regression half of
// #4243/#4417: a single-repo instance keeps routing to its only repo,
// whether or not the gaggle declares a project.
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
			_, recorder, notify := circuitBreakerRoutingRunnerConfig(t, cfg, project, want, "run-single-"+name, "9")
			if err := notify(journal.PhaseEscalated); err != nil {
				t.Fatalf("notify: %v", err)
			}
			assertCircuitBreakerRoutedTo(t, recorder, want)
		})
	}
}

// TestTerminalCircuitBreakerConcurrentRunsDoNotCrossRoute is #4417's own
// acceptance criterion: "multi-repository instances have regression coverage
// proving simultaneous runs cannot cross-route identifiers." Two runs under
// the SAME gaggle each claim an item from a DIFFERENT repo — the shape
// #4243's gaggle-level fix never covered, since both runs share one gaggle
// and therefore one declared project. Before #4417, applyCircuitBreaker
// applied a single repo argument to every item a call touched; with two
// concurrent runs sharing that argument's derivation, one run's terminal
// could mutate the other run's item on the wrong repo. Each run's own
// recorded item identity must be the only thing that decides where its
// terminal call lands.
func TestTerminalCircuitBreakerConcurrentRunsDoNotCrossRoute(t *testing.T) {
	t.Setenv("CB_ROUTE_TOK_WEB", "web-token-value")
	t.Setenv("CB_ROUTE_TOK_API", "api-token-value")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_WEB"}},
		{Provider: "github", Owner: "acme", Name: "api", Token: instance.TokenRef{Env: "CB_ROUTE_TOK_API"}},
	}}
	webRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	apiRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}

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
	const (
		webRunID, webItemID = "run-web", "70"
		apiRunID, apiItemID = "run-api", "71"
	)
	if ok, _, err := ledger.Claim(webItemID, webRunID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed web claim: ok=%v err=%v", ok, err)
	}
	seedItemRepositoryForTest(t, l, webRunID, webItemID, webRepo)
	if ok, _, err := ledger.Claim(apiItemID, apiRunID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed api claim: ok=%v err=%v", ok, err)
	}
	seedItemRepositoryForTest(t, l, apiRunID, apiItemID, apiRepo)

	runnerCfg, _, err := buildRunnerConfig(runnerCompositionInput{
		Layout:         l,
		Config:         cfg,
		SharedRegistry: journal.NewRegistryScrubber(),
		GaggleProject:  apiv1.RepoRef{}, // one gaggle, no declared project — both runs share it
		SandboxPosture: instance.SandboxDisabled,
	})
	if err != nil {
		t.Fatalf("buildRunnerConfig: %v", err)
	}
	if runnerCfg.NotifyTerminal == nil {
		t.Fatal("expected a wired terminal notifier")
	}

	if err := runnerCfg.NotifyTerminal(webRunID, journal.PhaseEscalated, "open-pr-gate"); err != nil {
		t.Fatalf("notify web run: %v", err)
	}
	if err := runnerCfg.NotifyTerminal(apiRunID, journal.PhaseEscalated, "open-pr-gate"); err != nil {
		t.Fatalf("notify api run: %v", err)
	}

	if len(recorder.repos) == 0 {
		t.Fatal("circuit breaker made no provider calls to check")
	}
	for _, got := range recorder.repos {
		if got != webRepo && got != apiRepo {
			t.Fatalf("breaker routed to unexpected repo %+v", got)
		}
	}
	// The critical assertion: both repos actually received calls — proving
	// each run's own recorded identity, not a value shared across the two
	// concurrently-live claims, decided where its terminal call landed.
	webCalls, apiCalls := 0, 0
	for _, got := range recorder.repos {
		switch got {
		case webRepo:
			webCalls++
		case apiRepo:
			apiCalls++
		}
	}
	if webCalls == 0 || apiCalls == 0 {
		t.Fatalf("want provider calls routed to both repos, got web=%d api=%d (repos=%+v)", webCalls, apiCalls, recorder.repos)
	}
}
