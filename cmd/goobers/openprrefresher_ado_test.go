package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// recordingADOOpenPRLister is the fake ADO provider the ADO-N30 tests poll: it
// records the RepositoryRef each poll addresses and returns fixed PRs.
type recordingADOOpenPRLister struct {
	mu    sync.Mutex
	repos []providers.RepositoryRef
	prs   []providers.OpenPRSummary
}

func (f *recordingADOOpenPRLister) ListOpenPullRequests(_ context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repo)
	return f.prs, nil
}

func (f *recordingADOOpenPRLister) lastRepo() (providers.RepositoryRef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.repos) == 0 {
		return providers.RepositoryRef{}, false
	}
	return f.repos[len(f.repos)-1], true
}

// adoOpenPRDeps is productionRuntimeDeps with the ADO open-PR provider built
// by build.
func adoOpenPRDeps(build func(instance.RepoRef) (localscheduler.OpenPRLister, error)) runtimeDeps {
	deps := productionRuntimeDeps()
	deps.openPRListers.ado = func(repo instance.RepoRef, _ runner.SecretRegistrar, _ credentials.StoreResolver, _ providers.QuotaObserver) (localscheduler.OpenPRLister, error) {
		return build(repo)
	}
	return deps
}

func waitOpenPRCount(t *testing.T, counter localscheduler.OpenPRCounter, gaggle, workflow string) int {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if n, known := counter.OpenPRCount(gaggle, workflow); known {
			return n
		}
		select {
		case <-deadline:
			t.Fatalf("count for gaggle %q never became known", gaggle)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestBuildOpenPRRefresherEnforcesADOCap is ADO-N30: a capped gaggle bound to
// an Azure DevOps repository gets a refresher that polls that repository
// (addressed by project) through the configured repo's own auth, so the
// scheduler cap counts its run-branch PRs and drops the human-parked one.
func TestBuildOpenPRRefresherEnforcesADOCap(t *testing.T) {
	t.Parallel()
	adoRepo := instance.RepoRef{
		Provider: "ado", Owner: "example-org", Project: "example-project", Name: "web",
		Auth: &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI},
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{adoRepo}}
	workflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{
		Gaggle: "example", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 2},
	}}}
	projects := map[string]apiv1.RepoRef{
		"example": {Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"},
	}
	fake := &recordingADOOpenPRLister{prs: []providers.OpenPRSummary{
		{Head: "goobers/implementation/run-1"},
		{Head: "goobers/implementation/run-2"},
		{Head: "goobers/implementation/run-3", Labels: []string{"Goobers:Merge-Escalated"}},
		{Head: "feature/human"},
	}}
	var built []instance.RepoRef
	deps := adoOpenPRDeps(func(repo instance.RepoRef) (localscheduler.OpenPRLister, error) {
		built = append(built, repo)
		return fake, nil
	})

	set, err := deps.openPRRefresher(cfg, workflows, projects, &openPRTestRegistrar{}, nil, t.TempDir(), nil, "generation-test", nil)
	if err != nil {
		t.Fatalf("buildOpenPRRefresher: %v", err)
	}
	if set == nil {
		t.Fatal("an ADO-projected capped gaggle must get a refresher")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { set.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if n := waitOpenPRCount(t, set, "example", "implementation"); n != 2 {
		t.Fatalf("count = %d, want 2 (parked PR excluded, human branch ignored)", n)
	}
	repo, ok := fake.lastRepo()
	want := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"}
	if !ok || repo != want {
		t.Fatalf("polled repo = %#v, want %#v", repo, want)
	}
	if len(built) != 1 || built[0].Auth == nil || built[0].Auth.Kind != instance.ADOAuthAzureCLI {
		t.Fatalf("provider built from %#v, want the configured repo with its auth", built)
	}

	c := localscheduler.NewConditions()
	c.SetOpenPRCounter(set)
	if ok, reason := c.AdmitProviderWorkflow(localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "implementation"}, apiv1.ProviderADO, apiv1.ReadinessConditions{MaxConcurrentRuns: 10, MaxOpenPRs: 2}, time.Now()); ok || reason != localscheduler.ReasonOpenPRCap {
		t.Fatalf("Admit ok=%v reason=%q, want blocked with %q", ok, reason, localscheduler.ReasonOpenPRCap)
	}
}

// flakyADOOpenPRLister answers its first ListOpenPullRequests with prs and
// fails every later one, counting the calls.
type flakyADOOpenPRLister struct {
	mu    sync.Mutex
	calls int
	prs   []providers.OpenPRSummary
}

func (f *flakyADOOpenPRLister) ListOpenPullRequests(context.Context, providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls > 1 {
		return nil, errors.New("ado unavailable")
	}
	return f.prs, nil
}

// pollOpenPRRefresherOnce runs one eager poll of every refresher in set:
// Run polls once before it first waits, and returns at that wait when ctx is
// already done.
func pollOpenPRRefresherOnce(set *localscheduler.OpenPRRefresherSet) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	set.Run(ctx)
}

// TestBuildOpenPRRefresherADOFailsOpen pins the ruling for ADO-N30: a count
// that cannot be read stays unknown and Admit admits, the same fail-open
// behavior every provider has. An ADO project with no configured binding gets
// no refresher at all.
func TestBuildOpenPRRefresherADOFailsOpen(t *testing.T) {
	t.Parallel()
	workflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{
		Gaggle: "example", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 1},
	}}}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "ado", Owner: "example-org", Project: "example-project", Name: "web"},
	}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "implementation"}
	readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 10, MaxOpenPRs: 1}

	// Each poll is driven explicitly and counted, so "unknown" is asserted
	// after a poll that really ran and failed — never confused with the
	// initial unknown of a refresher that has not polled yet — and a failure
	// after a success is shown to reset a count that was known.
	t.Run("a failed poll leaves or resets the count unknown", func(t *testing.T) {
		lister := &flakyADOOpenPRLister{prs: []providers.OpenPRSummary{{Head: "goobers/implementation/run-1"}}}
		var builds int
		deps := adoOpenPRDeps(func(instance.RepoRef) (localscheduler.OpenPRLister, error) {
			builds++
			if builds == 1 {
				return nil, errors.New("no credential")
			}
			return lister, nil
		})
		projects := map[string]apiv1.RepoRef{
			"example": {Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"},
		}
		set, err := deps.openPRRefresher(cfg, workflows, projects, &openPRTestRegistrar{}, nil, t.TempDir(), nil, "generation-test", nil)
		if err != nil || set == nil {
			t.Fatalf("set = %v, err = %v; want a refresher", set, err)
		}
		c := localscheduler.NewConditions()
		c.SetOpenPRCounter(set)

		// Poll 1: the provider cannot be built.
		pollOpenPRRefresherOnce(set)
		if builds != 1 {
			t.Fatalf("provider builds after the first poll = %d, want 1 (the poll must have run)", builds)
		}
		if _, known := set.OpenPRCount("example", "implementation"); known {
			t.Fatal("a poll whose provider cannot be built must leave the count unknown")
		}
		if ok, reason := c.AdmitProviderWorkflow(identity, apiv1.ProviderADO, readiness, time.Now()); !ok {
			t.Fatalf("Admit ok=%v reason=%q, want admitted (fail-open)", ok, reason)
		}

		// Poll 2: the build is retried and the list succeeds, so the count
		// is known and the cap binds.
		pollOpenPRRefresherOnce(set)
		if n, known := set.OpenPRCount("example", "implementation"); !known || n != 1 {
			t.Fatalf("count after a successful poll = (%d, %v), want (1, true)", n, known)
		}
		if ok, reason := c.AdmitProviderWorkflow(identity, apiv1.ProviderADO, readiness, time.Now()); ok || reason != localscheduler.ReasonOpenPRCap {
			t.Fatalf("Admit ok=%v reason=%q, want blocked with %q", ok, reason, localscheduler.ReasonOpenPRCap)
		}

		// Poll 3: the list fails, which must drop the known count rather
		// than keep enforcing a stale one.
		pollOpenPRRefresherOnce(set)
		lister.mu.Lock()
		calls := lister.calls
		lister.mu.Unlock()
		if builds != 2 || calls != 2 {
			t.Fatalf("builds = %d, list calls = %d; want the built provider reused and polled twice", builds, calls)
		}
		if _, known := set.OpenPRCount("example", "implementation"); known {
			t.Fatal("a failed poll after a successful one must reset the count to unknown")
		}
		if ok, reason := c.AdmitProviderWorkflow(identity, apiv1.ProviderADO, readiness, time.Now()); !ok {
			t.Fatalf("Admit ok=%v reason=%q, want admitted (fail-open)", ok, reason)
		}
	})

	t.Run("unconfigured ADO project gets no refresher", func(t *testing.T) {
		deps := adoOpenPRDeps(func(instance.RepoRef) (localscheduler.OpenPRLister, error) {
			t.Fatal("no provider may be built for an unconfigured ADO project")
			return nil, nil
		})
		projects := map[string]apiv1.RepoRef{
			"example": {Provider: apiv1.ProviderADO, Owner: "example-org", Project: "other-project", Name: "site"},
		}
		set, err := deps.openPRRefresher(cfg, workflows, projects, &openPRTestRegistrar{}, nil, t.TempDir(), nil, "generation-test", nil)
		if err != nil || set != nil {
			t.Fatalf("set = %v, err = %v; want nil, nil", set, err)
		}
	})
}
