package schedcounters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type fakeClaims struct {
	listing claimsclient.Listing
	err     error
	reads   int
}

func (f *fakeClaims) ListNamespace(_ context.Context, gaggle, provider string) (claimsclient.Listing, error) {
	f.reads++
	if f.err != nil {
		return claimsclient.Listing{}, f.err
	}
	var out claimsclient.Listing
	for _, e := range f.listing.Entries {
		if claimsclient.InNamespace(e, gaggle, provider) {
			out.Entries = append(out.Entries, e)
		}
	}
	return out, nil
}

type fakeJournal struct {
	owners map[string]journalclient.BranchOwnershipResponse
	calls  []string
}

func (f *fakeJournal) BranchOwnership(_ context.Context, req journalclient.BranchOwnershipRequest) (journalclient.BranchOwnershipResponse, error) {
	f.calls = append(f.calls, req.Branch)
	return f.owners[req.Branch], nil
}

// demandGeneration is the schedule-demand fixture the local counter's own test
// uses: gaggle "goobers" on acme/web, a scheduled update-behind-pr workflow, PRs
// under the team/ prefix.
func demandGeneration() (*instance.Config, *instance.ConfigSet) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "goobers"}, Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		}}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "pr-remediation"},
			Spec: apiv1.WorkflowSpec{
				Gaggle: "goobers", ScheduleCatchUp: "all",
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}},
			},
		}},
	}
	startingWith(set, "goobers", "update-behind-pr")
	set.Workflows[0].Spec.Tasks[0].Inputs = map[string]string{"base": "main", "headPrefix": "team/"}
	return &instance.Config{Timezone: "UTC", Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web"}}}, set
}

var demandIdentity = localscheduler.WorkflowIdentity{Gaggle: "goobers", Workflow: "pr-remediation"}

func fixtureForge(t *testing.T, heads ...map[int]string) *httptest.Server {
	t.Helper()
	override := map[int]string{}
	if len(heads) > 0 {
		override = heads[0]
	}
	pr := func(n int, head string, labels ...string) map[string]any {
		if h, ok := override[n]; ok {
			head = h
		}
		var ls []map[string]string
		for _, l := range labels {
			ls = append(ls, map[string]string{"name": l})
		}
		return map[string]any{
			"number": n, "state": "open",
			"head": map[string]string{"ref": head, "sha": "sha-" + itoa(n)},
			"base": map[string]string{"ref": "main", "sha": "base"}, "labels": ls,
		}
	}
	prs := []map[string]any{
		pr(41, "team/implementation/41", providers.LabelNeedsRemediation),
		pr(42, "team/implementation/42"),
		pr(43, "team/implementation/43"),
		pr(44, "outside/44"),
		pr(45, "team/implementation/45", providers.LabelNeedsHuman),
	}
	states := map[string]string{"sha-41": "failure", "sha-42": "failure", "sha-43": "success", "sha-45": "failure"}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/web/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(prs)
	})
	mux.HandleFunc("/repos/acme/web/commits/", func(w http.ResponseWriter, r *http.Request) {
		sha := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/acme/web/commits/"), "/")[0]
		state := states[sha]
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": state, "statuses": []map[string]string{{"context": "required-ci", "state": state}}})
	})
	// The live base tip, and a compare whose merge base IS that tip: nothing is
	// behind its base, so only the strong signals count.
	mux.HandleFunc("/repos/acme/web/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "tip"}})
	})
	mux.HandleFunc("/repos/acme/web/compare/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"merge_base_commit": map[string]string{"sha": "tip"}, "files": []any{}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func demandCounters(t *testing.T, forge *httptest.Server, claims *fakeClaims, journal *fakeJournal, quota *localscheduler.ProviderQuotaState, now ...func() time.Time) (*Counters, *[]error) {
	t.Helper()
	cfg, set := demandGeneration()
	source := GitHubSource{
		Resolve: func(context.Context, string) (string, error) { return "token", nil },
		Options: []func(*providers.GitHubProvider){func(p *providers.GitHubProvider) { p.BaseURL = forge.URL }},
	}
	reported := &[]error{}
	deps := Deps{Config: cfg, Set: set, Open: source.Open, Quota: quota, Report: func(err error) { *reported = append(*reported, err) }}
	if len(now) > 0 {
		deps.Now = now[0]
	}
	if claims != nil {
		deps.Claims = claims
	}
	if journal != nil {
		deps.Journal = journal
	}
	c, err := newWithQuota(deps)
	if err != nil {
		t.Fatal(err)
	}
	return c, reported
}

func activeClaim(number int, expires time.Time) claimsclient.Entry {
	return claimsclient.Entry{
		ItemID: "pr/" + itoa(number), ExternalID: "pr/" + itoa(number), Gaggle: "goobers", Provider: string(providers.ProviderGitHub),
		RunID: "active-run", Workflow: "pr-remediation", ExpiresAt: expires,
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// The local counter's own fixture, through the cloud counter: #41 is claimed,
// #42 is the one unclaimed failing PR, #43 is green, #44 is outside the prefix and
// #45 needs a human. Agreeing with the local count on identical forge responses
// is the point of sharing the selection.
func TestScheduleDemandAgreesWithTheLocalCounterOnTheSharedFixture(t *testing.T) {
	t.Parallel()
	claims := &fakeClaims{listing: claimsclient.Listing{Entries: []claimsclient.Entry{activeClaim(41, time.Now().Add(time.Hour))}}}
	c, reported := demandCounters(t, fixtureForge(t), claims, &fakeJournal{}, nil)
	counter := c.For(demandIdentity).ScheduleDemand
	if counter == nil {
		t.Fatalf("no schedule demand counter was built: %v", *reported)
	}
	if n, err := counter.EligibleCount(t.Context()); err != nil || n != 1 {
		t.Fatalf("eligible = %d (%v), want only the unclaimed failing PR #42", n, err)
	}
}

func TestAnExpiredClaimNoLongerHidesAPullRequest(t *testing.T) {
	t.Parallel()
	claims := &fakeClaims{listing: claimsclient.Listing{Entries: []claimsclient.Entry{activeClaim(41, time.Now().Add(-time.Minute))}}}
	c, _ := demandCounters(t, fixtureForge(t), claims, &fakeJournal{}, nil)
	if n, err := c.For(demandIdentity).ScheduleDemand.EligibleCount(t.Context()); err != nil || n != 2 {
		t.Fatalf("eligible = %d (%v), want #41 (expired claim) and #42", n, err)
	}
}

// A run still working a PR's run-scoped branch fences it. Ownership is read
// through the cloud journal, not a local file. A terminal origin does not fence.
func TestALiveOriginRunFencesItsPullRequest(t *testing.T) {
	t.Parallel()
	const live, done = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	liveHead, doneHead := "team/implementation/"+live, "team/implementation/"+done
	forge := fixtureForge(t, map[int]string{42: liveHead, 43: doneHead})
	j := &fakeJournal{owners: map[string]journalclient.BranchOwnershipResponse{
		liveHead: {Owner: &journalclient.BranchOwnership{RunID: live, Workflow: "implementation", Phase: string(journal.PhaseRunning)}},
		doneHead: {Owner: &journalclient.BranchOwnership{RunID: done, Workflow: "implementation", Phase: string(journal.PhaseCompleted)}},
	}}
	claims := &fakeClaims{listing: claimsclient.Listing{Entries: []claimsclient.Entry{activeClaim(41, time.Now().Add(time.Hour))}}}
	c, _ := demandCounters(t, forge, claims, j, nil)
	n, err := c.For(demandIdentity).ScheduleDemand.EligibleCount(t.Context())
	if err != nil || n != 0 {
		t.Fatalf("eligible = %d (%v), want 0: #42's origin run is live, #41 is claimed, #43 is green", n, err)
	}
	if len(j.calls) == 0 {
		t.Fatal("the cloud journal was never asked who owns the run-scoped branches")
	}
}

func TestAClaimReadFailureFailsThePollInsteadOfCountingEverything(t *testing.T) {
	t.Parallel()
	boom := errors.New("claims plane unavailable")
	c, _ := demandCounters(t, fixtureForge(t), &fakeClaims{err: boom}, &fakeJournal{}, nil)
	if n, err := c.For(demandIdentity).ScheduleDemand.EligibleCount(t.Context()); !errors.Is(err, boom) || n != 0 {
		t.Fatalf("eligible = %d (%v), want the claim read failure and no count", n, err)
	}
}

// The counter exists only with the planes it needs. Without them the workflow
// keeps its pending reason, loudly, instead of counting against nothing.
func TestWithoutClaimsAndJournalTheScheduledWorkflowStaysDisabledAndSaysWhy(t *testing.T) {
	t.Parallel()
	c, reported := demandCounters(t, fixtureForge(t), nil, nil, nil)
	counters := c.For(demandIdentity)
	if counters.ScheduleDemand != nil {
		t.Fatal("a counter was built without a claims namespace")
	}
	if len(*reported) != 1 || !strings.Contains((*reported)[0].Error(), "claims namespace") {
		t.Fatalf("reported = %v", *reported)
	}
	cfg, set := demandGeneration()
	pins := map[string]cloudconfig.Pins{"goobers/pr-remediation": {WorkflowDigest: "w", GooberDigest: "g"}}
	entries, err := schedentries.Build(schedentries.Input{Generation: "g", Config: cfg, Set: set, Pins: pins, Counters: c.For})
	if err != nil || entries[0].DisabledReason != schedentries.ScheduleDemandPendingReason {
		t.Fatalf("entry reason = %q (%v), want the schedule-demand pending reason", entries[0].DisabledReason, err)
	}
}

func TestSuppliedScheduleDemandEnablesTheEntry(t *testing.T) {
	t.Parallel()
	c, _ := demandCounters(t, fixtureForge(t), &fakeClaims{}, &fakeJournal{}, nil)
	cfg, set := demandGeneration()
	pins := map[string]cloudconfig.Pins{"goobers/pr-remediation": {WorkflowDigest: "w", GooberDigest: "g"}}
	entries, err := schedentries.Build(schedentries.Input{Generation: "g", Config: cfg, Set: set, Pins: pins, Counters: c.For})
	if err != nil || entries[0].DisabledReason != "" || entries[0].ScheduleDemandCounter == nil {
		t.Fatalf("entry = reason %q counter %v (%v), want enabled with the counter", entries[0].DisabledReason, entries[0].ScheduleDemandCounter, err)
	}
}

// Azure DevOps has no update-behind remediation, so no counter and no pending
// reason: each due tick fires unsized there, exactly as locally.
func TestNoScheduleDemandCounterForAzureDevOpsOrAnUnscheduledWorkflow(t *testing.T) {
	t.Parallel()
	forge := fixtureForge(t)
	for name, mutate := range map[string]func(*instance.Config, *instance.ConfigSet){
		"azure devops": func(c *instance.Config, s *instance.ConfigSet) {
			s.Gaggles[0].Spec.Project.Provider = apiv1.ProviderADO
			c.Repos = []instance.RepoRef{{Provider: "ado", Owner: "acme", Project: "p", Name: "web"}}
		},
		"no schedule trigger": func(_ *instance.Config, s *instance.ConfigSet) { s.Workflows[0].Spec.Triggers = nil },
		"another first stage": func(_ *instance.Config, s *instance.ConfigSet) { startingWith(s, "goobers", "other") },
		"no repositories":     func(c *instance.Config, _ *instance.ConfigSet) { c.Repos = nil },
	} {
		cfg, set := demandGeneration()
		mutate(cfg, set)
		c, err := newWithQuota(Deps{Config: cfg, Set: set, Open: GitHubSource{}.Open, Claims: &fakeClaims{}, Journal: &fakeJournal{}, Report: func(err error) { t.Errorf("%s: unexpected report: %v", name, err) }})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.For(demandIdentity).ScheduleDemand != nil {
			t.Errorf("%s: built a counter the local daemon does not", name)
		}
	}
	_ = forge
}

// The counter spends the same ledger as every other poll: an exhausted window
// sheds it before the first request.
func TestScheduleDemandIsShedByAnExhaustedQuotaWindow(t *testing.T) {
	t.Parallel()
	forge := fixtureForge(t)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, time.Now().Add(time.Hour).Truncate(time.Second))
	c, _ := demandCounters(t, forge, &fakeClaims{}, &fakeJournal{}, quota)
	counter := c.For(demandIdentity).ScheduleDemand
	guarded, ok := counter.(localscheduler.ProviderQuotaGuardedBacklogCounter)
	if !ok || !guarded.ProviderQuotaGuarded() {
		t.Fatal("the counter does not advertise quota guarding")
	}
	var budget *localscheduler.ProviderPollBudgetError
	if _, err := counter.EligibleCount(t.Context()); !errors.As(err, &budget) {
		t.Fatalf("EligibleCount = %v, want a poll budget error", err)
	}
}

// The cloud control role never executes a stage: nothing here may reach the
// runner, which is why the shared selection lives in its own package.
func TestPackageClosureExcludesTheRunner(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/goobers/goobers/internal/cloud/schedcounters").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "github.com/goobers/goobers/internal/runner" || strings.HasPrefix(dep, "github.com/goobers/goobers/cmd/") {
			t.Fatalf("internal/cloud/schedcounters depends on %s", dep)
		}
	}
}

// Claim expiry is judged against Deps.Now, never the wall clock: the same claim
// is live at one injected instant and expired at another, whatever the host
// clock says.
func TestClaimExpiryFollowsTheInjectedClockNotTheWallClock(t *testing.T) {
	t.Parallel()
	expires := time.Now().Add(time.Hour) // live on the wall clock
	for name, tc := range map[string]struct {
		now  time.Time
		want int
	}{
		"before the claim expires, by the injected clock": {now: expires.Add(-30 * time.Minute), want: 1},
		"after the claim expires, by the injected clock":  {now: expires.Add(time.Hour), want: 2},
	} {
		now := tc.now
		claims := &fakeClaims{listing: claimsclient.Listing{Entries: []claimsclient.Entry{activeClaim(41, expires)}}}
		c, _ := demandCounters(t, fixtureForge(t), claims, &fakeJournal{}, nil, func() time.Time { return now })
		if n, err := c.For(demandIdentity).ScheduleDemand.EligibleCount(t.Context()); err != nil || n != tc.want {
			t.Errorf("%s: eligible = %d (%v), want %d", name, n, err, tc.want)
		}
	}
}

// With no configured repository there is nothing to count against, so no counter
// is built and nothing is indexed out of range. The workflow-level guard in For
// makes this unreachable through it, so the check is pinned directly.
func TestScheduleDemandNeedsAConfiguredRepository(t *testing.T) {
	t.Parallel()
	cfg, set := demandGeneration()
	cfg.Repos = nil
	c, err := newWithQuota(Deps{Config: cfg, Set: set, Open: GitHubSource{}.Open, Claims: &fakeClaims{}, Journal: &fakeJournal{}})
	if err != nil {
		t.Fatal(err)
	}
	demand, err := c.scheduleDemand(set.Gaggles[0], &set.Workflows[0])
	if demand != nil || err != nil {
		t.Fatalf("scheduleDemand without repositories = %v, %v; want no counter and no error", demand, err)
	}
}
