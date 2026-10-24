package schedcounters

import (
	"context"
	"errors"
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

func opener(f *fakeProvider) func(context.Context, providers.RepositoryRef) (Provider, error) {
	return func(context.Context, providers.RepositoryRef) (Provider, error) { return f, nil }
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
	c, err := New(Deps{Config: cfg, Set: set, Open: func(context.Context, providers.RepositoryRef) (Provider, error) { return nil, boom }})
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
