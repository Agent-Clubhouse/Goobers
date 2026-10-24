package schedcounters

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type fakeProvider struct {
	mu       sync.Mutex
	items    []providers.WorkItem
	prs      []providers.OpenPRSummary
	requests []providers.ListWorkItemsRequest
	pages    []providers.ListWorkItemsPageInfo // one per call, consumed in order
	prCalls  atomic.Int32
	prErr    error
}

func (f *fakeProvider) ListWorkItems(_ context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.pages) > 0 {
		*req.PageInfo = f.pages[0]
		f.pages = f.pages[1:]
	}
	return f.items, nil
}

func (f *fakeProvider) ListOpenPullRequests(context.Context, providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	f.prCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs, f.prErr
}

func opener(f *fakeProvider) Opener {
	return func(context.Context, providers.RepositoryRef, Access) (Provider, error) { return f, nil }
}

func generation(triggers ...apiv1.Trigger) (*instance.Config, *instance.ConfigSet) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}, Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "o", Name: "r", Branch: "main"},
		}}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "wf"},
			Spec:       apiv1.WorkflowSpec{Gaggle: "g", Triggers: triggers, ScheduleCatchUp: "all"},
		}},
	}
	return &instance.Config{Timezone: "UTC", Repos: []instance.RepoRef{{Provider: "github", Owner: "o", Name: "r"}}}, set
}

func startingWith(set *instance.ConfigSet, command ...string) {
	set.Workflows[0].Spec.Start = "first"
	set.Workflows[0].Spec.Tasks = []apiv1.Task{{Name: "first", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: command}}}
}

var identity = localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "wf"}

func newCounters(t *testing.T, cfg *instance.Config, set *instance.ConfigSet, f *fakeProvider) *Counters {
	t.Helper()
	c, err := New(Deps{Config: cfg, Set: set, Open: opener(f), SelfIdentity: "bot", Report: func(err error) { t.Errorf("unexpected report: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reasonWith(t *testing.T, cfg *instance.Config, set *instance.ConfigSet, c *Counters) string {
	t.Helper()
	pins := map[string]cloudconfig.Pins{"g/wf": {WorkflowDigest: "w", GooberDigest: "g"}}
	entries, err := schedentries.Build(schedentries.Input{Generation: "gen", Config: cfg, Set: set, Pins: pins, Counters: c.For})
	if err != nil {
		t.Fatal(err)
	}
	return entries[0].DisabledReason
}

// The counters this package supplies clear exactly their own pending reasons: a
// workflow needing a counter this package does not yet supply (schedule demand)
// stays disabled with that reason alone.
func TestSuppliedCountersClearOnlyTheirOwnPendingReason(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}})
	if got := reasonWith(t, cfg, set, newCounters(t, cfg, set, &fakeProvider{})); got != "" {
		t.Fatalf("backlog workflow still disabled: %q", got)
	}

	cfg, set = generation(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 2
	startingWith(set, "goobers", "backlog-query")
	if got := reasonWith(t, cfg, set, newCounters(t, cfg, set, &fakeProvider{})); got != "" {
		t.Fatalf("refill workflow still disabled: %q", got)
	}

	cfg, set = generation(apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"})
	startingWith(set, "goobers", "update-behind-pr")
	if got := reasonWith(t, cfg, set, newCounters(t, cfg, set, &fakeProvider{})); got != schedentries.ScheduleDemandPendingReason {
		t.Fatalf("scheduled remediation reason = %q, want only the schedule-demand reason", got)
	}

	// Backlog and schedule demand on one workflow: only the backlog reason clears.
	cfg, set = generation(
		apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}},
		apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"})
	startingWith(set, "goobers", "update-behind-pr")
	if got := reasonWith(t, cfg, set, newCounters(t, cfg, set, &fakeProvider{})); got != schedentries.ScheduleDemandPendingReason {
		t.Fatalf("mixed workflow reason = %q, want only the schedule-demand reason", got)
	}
}

func TestBacklogCounterCountsEligibleItemsAndTracksOldestReady(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}})
	older, newer := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeProvider{items: []providers.WorkItem{
		{ID: "1", Labels: []string{"ready"}, ReadyAt: &newer},
		{ID: "2", Labels: []string{"ready"}, ReadyAt: &older},
		{ID: "3", Labels: []string{"other"}},
	}}
	counter := newCounters(t, cfg, set, f).For(identity).Backlog
	snapshot := counter.(interface {
		EligibleSnapshot(context.Context) (localscheduler.BacklogSnapshot, error)
	})
	got, err := snapshot.EligibleSnapshot(t.Context())
	if err != nil || got.Count != 2 || !got.OldestReadyAt.Equal(older) {
		t.Fatalf("snapshot = %+v (%v), want 2 eligible, oldest %v", got, err, older)
	}
	if req := f.requests[0]; req.State != "open" || !req.OldestFirst || len(req.Labels) != 1 || req.Labels[0] != "ready" {
		t.Fatalf("request = %+v", req)
	}
}

func TestBacklogCounterResumesAPagedScanByCursor(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}})
	f := &fakeProvider{pages: []providers.ListWorkItemsPageInfo{{HasNext: true, NextCursor: "c1"}, {}}}
	counter := newCounters(t, cfg, set, f).For(identity).Backlog
	for range 2 {
		if _, err := counter.EligibleCount(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.requests[0].Cursor != "" || f.requests[1].Cursor != "c1" {
		t.Fatalf("cursors = %q, %q; want a fresh scan then a resume", f.requests[0].Cursor, f.requests[1].Cursor)
	}
	if _, err := counter.EligibleCount(t.Context()); err != nil || f.requests[2].Cursor != "" {
		t.Fatalf("a finished scan must restart: cursor %q (%v)", f.requests[2].Cursor, err)
	}
}

func TestRefillCounterReadsTheBacklogQueryStagesOwnSelection(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 2
	startingWith(set, "goobers", "backlog-query")
	set.Workflows[0].Spec.Tasks[0].Inputs = map[string]string{
		"requireLabels": "ready", "excludeLabels": "wip", "parkLabels": "parked", "respectAssignee": "true",
	}
	f := &fakeProvider{items: []providers.WorkItem{
		{ID: "ok", Labels: []string{"ready"}, Assignee: "bot"},
		{ID: "wip", Labels: []string{"ready", "wip"}, Assignee: "bot"},
		{ID: "claimed", Labels: []string{"ready", providers.LabelClaimed}, Assignee: "bot"},
		{ID: "parked", Labels: []string{"ready", "parked"}, Assignee: "bot"},
		{ID: "elsewhere", Labels: []string{"ready"}, Assignee: "human"},
	}}
	counter := newCounters(t, cfg, set, f).For(identity).RefillDemand
	if n, err := counter.EligibleCount(t.Context()); err != nil || n != 1 {
		t.Fatalf("refill eligible = %d (%v), want 1", n, err)
	}
	if f.requests[0].Assignee != "bot" {
		t.Fatalf("respectAssignee must default to the controller identity, got %q", f.requests[0].Assignee)
	}

	set.Workflows[0].Spec.Tasks[0].Inputs["filterParkLabels"] = "false"
	f.requests = nil
	counter = newCounters(t, cfg, set, f).For(identity).RefillDemand
	if n, _ := counter.EligibleCount(t.Context()); n != 2 {
		t.Fatalf("with the park filter off the parked item must count: %d, want 2", n)
	}
}

func TestAnUncompilableSelectionIsReportedAndLeavesTheWorkflowDisabled(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 2
	startingWith(set, "goobers", "backlog-query")
	set.Workflows[0].Spec.Tasks[0].Inputs = map[string]string{"filterParkLabels": "maybe"}
	var reported []error
	c, err := New(Deps{Config: cfg, Set: set, Open: opener(&fakeProvider{}), Report: func(err error) { reported = append(reported, err) }})
	if err != nil {
		t.Fatal(err)
	}
	if got := reasonWith(t, cfg, set, c); got != schedentries.RefillPendingReason {
		t.Fatalf("reason = %q, want the refill pending reason", got)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "filterParkLabels") {
		t.Fatalf("reported = %v", reported)
	}
}

func TestAProviderThatCannotBeOpenedFailsThePollNotTheController(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}})
	boom := errors.New("no credential")
	c, err := New(Deps{Config: cfg, Set: set, Open: func(context.Context, providers.RepositoryRef, Access) (Provider, error) { return nil, boom }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.For(identity).Backlog.EligibleCount(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("EligibleCount = %v, want the credential failure", err)
	}
}

func cappedGeneration() (*instance.Config, *instance.ConfigSet) {
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Workflows[0].Spec.Readiness.MaxOpenPRs = 3
	return cfg, set
}

func TestOpenPRCountIsUnknownUntilTheFirstPollThenCountsOwnBranchesOnly(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{
		{Head: "goobers/wf/a"}, {Head: "goobers/wf/b"},
		{Head: "goobers/wf/parked", Labels: []string{providers.LabelMergeEscalated}},
		{Head: "human/feature"},
	}}
	c := newCounters(t, cfg, set, f)
	if _, known := c.OpenPRs().OpenPRCount("g", "wf"); known {
		t.Fatal("count must be unknown before the first poll so admission fails open")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { _, known := c.OpenPRs().OpenPRCount("g", "wf"); return known })
	if n, _ := c.OpenPRs().OpenPRCount("g", "wf"); n != 2 {
		t.Fatalf("open PR count = %d, want 2 (parked and human PRs excluded)", n)
	}
	if _, known := c.OpenPRs().OpenPRCount("other", "wf"); known {
		t.Fatal("a gaggle with no refresher must read unknown, never another repo's count")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestNoCappedWorkflowMeansNoRefresherAndRunJustWaitsForTheEpoch(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerManual})
	f := &fakeProvider{}
	c := newCounters(t, cfg, set, f)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case <-done:
		t.Fatal("Run returned while its epoch was live")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil || f.prCalls.Load() != 0 {
		t.Fatalf("Run = %v after %d polls, want a clean stop with none", err, f.prCalls.Load())
	}
}

// Many scheduler goroutines read the counter while the refresher publishes
// under the race detector.
func TestOpenPRCountIsSafeUnderConcurrentReadersAndRefresh(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}}}
	c, err := New(Deps{Config: cfg, Set: set, Open: opener(f), OpenPRInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if n, known := c.OpenPRs().OpenPRCount("g", "wf"); known && n != 1 {
					t.Errorf("count = %d, want 1", n)
					return
				}
				c.For(identity)
			}
		}()
	}
	wg.Wait()
	waitFor(t, func() bool { return f.prCalls.Load() >= 3 })
	cancel()
	<-done
}

// A deposed controller stops polling when its epoch ends; the next leader's
// counters start unknown and poll for themselves, so a takeover never inherits
// a count the previous leader measured.
func TestTakeoverStopsTheOldEpochsPollingAndTheNewLeaderStartsUnknown(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}}}
	deps := Deps{Config: cfg, Set: set, Open: opener(f), OpenPRInterval: time.Millisecond}
	first, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, depose := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	waitFor(t, func() bool { _, known := first.OpenPRs().OpenPRCount("g", "wf"); return known })
	depose()
	<-done
	settled := f.prCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if f.prCalls.Load() != settled {
		t.Fatal("the deposed epoch kept polling")
	}

	second, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, known := second.OpenPRs().OpenPRCount("g", "wf"); known {
		t.Fatal("a new leader must not inherit the old leader's count")
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	done2 := make(chan error, 1)
	go func() { done2 <- second.Run(ctx2) }()
	waitFor(t, func() bool { _, known := second.OpenPRs().OpenPRCount("g", "wf"); return known })
	cancel2()
	<-done2
}

func TestAnUnsupportedCappedRepositoryRefusesTheGeneration(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	cfg.Repos = []instance.RepoRef{{Provider: "gitea", Owner: "o", Name: "r"}}
	set.Gaggles[0].Spec.Project.Provider = "gitea"
	if _, err := New(Deps{Config: cfg, Set: set, Open: opener(&fakeProvider{})}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("New = %v, want the unsupported provider named", err)
	}
}

func TestNewRequiresItsInputs(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New accepted empty deps")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// forge is a GitHub stand-in that reports a rate-limit window on every response.
type forge struct {
	*httptest.Server
	requests  atomic.Int32
	remaining int
	reset     time.Time
}

func newForge(t *testing.T, remaining int, reset time.Time) *forge {
	t.Helper()
	f := &forge{remaining: remaining, reset: reset}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(f.remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.reset.Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *forge) source() GitHubSource {
	return GitHubSource{
		Resolve: func(context.Context, string) (string, error) { return "token", nil },
		Options: []func(*providers.GitHubProvider){func(p *providers.GitHubProvider) { p.BaseURL = f.URL }},
	}
}

func backlogCounterOver(t *testing.T, quota *localscheduler.ProviderQuotaState, source GitHubSource) localscheduler.BacklogCounter {
	t.Helper()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}})
	c, err := New(Deps{Config: cfg, Set: set, Open: source.Open, Quota: quota})
	if err != nil {
		t.Fatal(err)
	}
	return c.For(identity).Backlog
}

// An exhausted window sheds the poll before any request is sent: the quota gate
// is inside the provider, so even a caller that skipped the scheduler's own
// reservation cannot spend quota the ledger says is gone.
func TestAnExhaustedQuotaWindowShedsThePollBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	f := newForge(t, 4000, reset)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, reset)
	counter := backlogCounterOver(t, quota, f.source())
	var budget *localscheduler.ProviderPollBudgetError
	if _, err := counter.EligibleCount(t.Context()); !errors.As(err, &budget) {
		t.Fatalf("EligibleCount = %v, want a poll budget error", err)
	}
	if f.requests.Load() != 0 {
		t.Fatalf("an exhausted window still sent %d request(s)", f.requests.Load())
	}
}

// A poll feeds the ledger the window it saw and spends exactly one unit of it.
func TestAPollSpendsOneUnitAndRecordsTheObservedWindow(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	f := newForge(t, 3, reset)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 5, reset)
	counter := backlogCounterOver(t, quota, f.source())
	if _, err := counter.EligibleCount(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 100).Allowed; got != 3 || f.requests.Load() != 1 {
		t.Fatalf("remaining after the poll = %d with %d request(s), want 3 and 1", got, f.requests.Load())
	}
}

// The scheduler pays for an admitted poll up front and hands the reservation in
// the context; the request must use it rather than reserve a second unit.
func TestAPrepaidReservationIsNotChargedTwice(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	f := newForge(t, 4, reset)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 5, reset)
	counter := backlogCounterOver(t, quota, f.source())
	budget := quota.ReservePolls(apiv1.ProviderGitHub, time.Now(), 1)
	if _, err := counter.EligibleCount(localscheduler.WithProviderPollBudget(t.Context(), budget)); err != nil {
		t.Fatal(err)
	}
	if got := quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 100).Allowed; got != 4 {
		t.Fatalf("remaining after a prepaid poll = %d, want 4 (charged twice would leave 3)", got)
	}
}

// A poll the provider never sent (a failed credential, a cache hit) hands its
// prepaid unit back.
func TestAnUnspentPrepaidReservationIsRefunded(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 5, reset)
	source := GitHubSource{Resolve: func(context.Context, string) (string, error) { return "", errors.New("no credential") }}
	counter := backlogCounterOver(t, quota, source)
	budget := quota.ReservePolls(apiv1.ProviderGitHub, time.Now(), 1)
	if _, err := counter.EligibleCount(localscheduler.WithProviderPollBudget(t.Context(), budget)); err == nil {
		t.Fatal("EligibleCount succeeded without a credential")
	}
	if got := quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 100).Allowed; got != 5 {
		t.Fatalf("remaining after an unspent reservation = %d, want 5", got)
	}
}

func TestCountersAdvertiseQuotaGuardingOnlyWithALedger(t *testing.T) {
	t.Parallel()
	for name, quota := range map[string]*localscheduler.ProviderQuotaState{"with a ledger": localscheduler.NewProviderQuotaState(), "without": nil} {
		counter := backlogCounterOver(t, quota, GitHubSource{})
		guarded, ok := counter.(localscheduler.ProviderQuotaGuardedBacklogCounter)
		if !ok || guarded.ProviderQuotaGuarded() != (quota != nil) {
			t.Errorf("%s: guarded = %v (implements %v)", name, ok && guarded.ProviderQuotaGuarded(), ok)
		}
	}
}

// consuming is an opener that, like the real provider, spends the poll's quota
// reservation through the gate before it answers.
func consuming(f *fakeProvider, opens *atomic.Int32) Opener {
	return func(ctx context.Context, repo providers.RepositoryRef, access Access) (Provider, error) {
		opens.Add(1)
		if access.Gate != nil {
			if err := access.Gate.AcquireQuotaRequest(ctx, repo.Provider); err != nil {
				return nil, err
			}
		}
		return f, nil
	}
}

// The open-PR refresher spends the same ledger. When the ledger sheds a poll the
// last list is served, so a quota shortage cannot turn into an unknown count
// that lifts the cap.
func TestTheRefresherSpendsQuotaAndKeepsItsLastCountWhenShed(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}, {Head: "goobers/wf/b"}}}
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 1, time.Now().Add(time.Hour))
	var opens atomic.Int32
	c, err := New(Deps{Config: cfg, Set: set, Open: consuming(f, &opens), Quota: quota, OpenPRInterval: time.Millisecond, OpenPRMaxStale: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { _, known := c.OpenPRs().OpenPRCount("g", "wf"); return known })
	// Many intervals pass with the window empty: no further provider call, and the
	// count stays known.
	settled := f.prCalls.Load()
	waitFor(t, func() bool { return quota.ReserveCurrentPolls(apiv1.ProviderGitHub, 1).Allowed == 0 })
	time.Sleep(20 * time.Millisecond)
	if n, known := c.OpenPRs().OpenPRCount("g", "wf"); !known || n != 2 || f.prCalls.Load() != settled || settled != 1 {
		t.Fatalf("after shedding: count %d known=%v with %d provider calls (settled at %d), want 2 known and exactly 1 call", n, known, f.prCalls.Load(), settled)
	}
	cancel()
	<-done
}

// Every poll opens exactly one provider: the credential is resolved per poll,
// and a poll never reuses (or leaks) another's accounting.
func TestEveryPollOpensExactlyOneProvider(t *testing.T) {
	t.Parallel()
	cfg, set := generation(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Selector: map[string]string{"ready": ""}})
	var opens atomic.Int32
	c, err := New(Deps{Config: cfg, Set: set, Open: consuming(&fakeProvider{}, &opens), Quota: localscheduler.NewProviderQuotaState()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := c.For(identity).Backlog.EligibleCount(t.Context()); err != nil {
			t.Fatal(err)
		}
		if int(opens.Load()) != i {
			t.Fatalf("after %d poll(s) Open ran %d time(s)", i, opens.Load())
		}
	}
	ccfg, cset := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}}}
	opens.Store(0)
	r, err := New(Deps{Config: ccfg, Set: cset, Open: consuming(f, &opens), OpenPRInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitFor(t, func() bool { return f.prCalls.Load() >= 3 })
	cancel()
	<-done
	if opens.Load() != f.prCalls.Load() {
		t.Fatalf("%d provider opens for %d refresher polls", opens.Load(), f.prCalls.Load())
	}
}

// A count measured in one epoch is never served in the next: Run resets the
// refreshers, so the new epoch reads unknown until its own first poll lands.
func TestRunResetsTheOpenPRCountForANewEpoch(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}}}
	gate := make(chan struct{})
	var blocked atomic.Bool
	c, err := New(Deps{Config: cfg, Set: set, OpenPRInterval: time.Millisecond, Open: func(ctx context.Context, _ providers.RepositoryRef, _ Access) (Provider, error) {
		if blocked.Load() {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	first, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(first) }()
	waitFor(t, func() bool { _, known := c.OpenPRs().OpenPRCount("g", "wf"); return known })
	stop()
	<-done
	if _, known := c.OpenPRs().OpenPRCount("g", "wf"); known {
		t.Fatal("a stopped epoch still serves its count")
	}

	blocked.Store(true)
	second, stop2 := context.WithCancel(t.Context())
	go func() { done <- c.Run(second) }()
	time.Sleep(20 * time.Millisecond)
	if _, known := c.OpenPRs().OpenPRCount("g", "wf"); known {
		t.Fatal("a new epoch served the previous epoch's count before its first poll")
	}
	close(gate)
	waitFor(t, func() bool { _, known := c.OpenPRs().OpenPRCount("g", "wf"); return known })
	stop2()
	<-done
}

// A rate-limited response fails the poll at once. Sleeping the limit out would
// hold a scheduler tick for minutes; the next reset-aware admission retries.
func TestARateLimitedPollFailsImmediatelyInsteadOfSleepingInsideTheTick(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	source := GitHubSource{
		Resolve: func(context.Context, string) (string, error) { return "token", nil },
		Options: []func(*providers.GitHubProvider){func(p *providers.GitHubProvider) { p.BaseURL = server.URL }},
	}
	counter := backlogCounterOver(t, localscheduler.NewProviderQuotaState(), source)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := counter.EligibleCount(ctx); err == nil || ctx.Err() != nil || requests.Load() != 1 {
		t.Fatalf("err=%v ctx=%v after %d request(s), want a prompt failure after exactly one", err, ctx.Err(), requests.Load())
	}
}

// Serving the last list while shed is bounded: a window reporting a bogus
// far-future reset must not freeze the cap on an old count for hours.
func TestAShedRefresherStopsServingItsLastListAfterTheBound(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reset   time.Duration // window reset, from the first poll
		advance time.Duration // clock movement before the shed poll
		stale   bool
	}{
		"inside both bounds":                 {reset: time.Hour, advance: 2 * time.Second, stale: false},
		"bogus far-future reset":             {reset: 1000 * time.Hour, advance: (maxStaleIntervals + 1) * 45 * time.Second, stale: true},
		"reset sooner than the interval cap": {reset: 30 * time.Second, advance: 31 * time.Second, stale: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clock := start
			quota := localscheduler.NewProviderQuotaState()
			quota.Record(apiv1.ProviderGitHub, 1, start.Add(tc.reset))
			var opens atomic.Int32
			l := newLister(Deps{Open: consuming(&fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}}}, &opens), Quota: quota})
			l.now = func() time.Time { return clock }
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "r"}
			if _, err := l.ListOpenPullRequests(t.Context(), repo); err != nil {
				t.Fatal(err)
			}
			clock = start.Add(tc.advance)
			prs, err := l.ListOpenPullRequests(t.Context(), repo)
			var budget *localscheduler.ProviderPollBudgetError
			switch {
			case tc.stale && !errors.As(err, &budget):
				t.Fatalf("a shed poll past the bound returned %v %v, want the budget error", prs, err)
			case !tc.stale && (err != nil || len(prs) != 1):
				t.Fatalf("a shed poll inside the bound returned %v %v, want the last list", prs, err)
			}
			if opens.Load() != 1 {
				t.Fatalf("a shed poll still opened a provider (%d opens)", opens.Load())
			}
		})
	}
}

// The scheduler retires an expired window on its tick; the refresher must then
// refresh from the provider again instead of holding the list it kept while shed.
func TestTheRefresherRefreshesAfterTheSchedulerRetiresTheWindow(t *testing.T) {
	t.Parallel()
	cfg, set := cappedGeneration()
	f := &fakeProvider{prs: []providers.OpenPRSummary{{Head: "goobers/wf/a"}}}
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	quota := localscheduler.NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 1, reset)
	var opens atomic.Int32
	c, err := New(Deps{Config: cfg, Set: set, Open: consuming(f, &opens), Quota: quota, OpenPRInterval: time.Millisecond, OpenPRMaxStale: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { n, known := c.OpenPRs().OpenPRCount("g", "wf"); return known && n == 1 })

	// The forge now has more open PRs, but the window is empty: still the old count.
	f.mu.Lock()
	f.prs = []providers.OpenPRSummary{{Head: "goobers/wf/a"}, {Head: "goobers/wf/b"}, {Head: "goobers/wf/c"}}
	f.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if n, _ := c.OpenPRs().OpenPRCount("g", "wf"); n != 1 {
		t.Fatalf("count moved to %d while the window was exhausted", n)
	}

	// The scheduler's tick retires the window once its reset has passed.
	if budget := quota.ReservePolls(apiv1.ProviderGitHub, reset.Add(time.Second), 1); !budget.Reset {
		t.Fatalf("the window was not retired: %+v", budget)
	}
	waitFor(t, func() bool { n, _ := c.OpenPRs().OpenPRCount("g", "wf"); return n == 3 })
	cancel()
	<-done
}
