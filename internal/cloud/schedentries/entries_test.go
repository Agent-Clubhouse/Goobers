package schedentries

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type noopStarter struct{}

func (noopStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return localscheduler.StartResult{}, nil
}

func starters(localscheduler.WorkflowIdentity) localscheduler.Starter { return noopStarter{} }

func off() *bool { v := false; return &v }

func fixture(triggers ...apiv1.Trigger) (*instance.Config, *instance.ConfigSet) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "o", Name: "r", Branch: "main"}}}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "wf"},
			Spec:       apiv1.WorkflowSpec{Gaggle: "g", Triggers: triggers, ScheduleCatchUp: "all"},
		}},
	}
	return &instance.Config{Timezone: "America/New_York"}, set
}

func pins(set *instance.ConfigSet) map[string]cloudconfig.Pins {
	out := map[string]cloudconfig.Pins{}
	for _, w := range set.Workflows {
		out[w.Spec.Gaggle+"/"+w.Name] = cloudconfig.Pins{WorkflowDigest: "wf-digest", GooberDigest: "goober-digest"}
	}
	return out
}

func build(t *testing.T, cfg *instance.Config, set *instance.ConfigSet) []localscheduler.WorkflowEntry {
	t.Helper()
	entries, err := Build(Input{Generation: "sha256:gen", Config: cfg, Set: set, Pins: pins(set), Starter: starters})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestBuildFillsTheEntryFromTheAppliedGeneration(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(
		apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 9 * * *", Priority: 3},
		apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "*/15 * * * *", Priority: 7},
		apiv1.Trigger{Type: apiv1.TriggerSignal, Signal: "deploy"},
		apiv1.Trigger{Type: apiv1.TriggerWebhook, Events: []string{"push"}},
		apiv1.Trigger{Type: apiv1.TriggerManual},
	)
	entries := build(t, cfg, set)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.ConfigGeneration != "sha256:gen" || e.WorkflowDigest != "wf-digest" || e.GooberDigest != "goober-digest" || e.WorkflowVersion != 1 {
		t.Fatalf("pins/generation = %+v", e)
	}
	if e.Gaggle != "g" || e.Workflow != "wf" || e.RepoRef.Name != "r" || e.Starter == nil || !e.ScheduleCatchUpAll {
		t.Fatalf("identity = %+v", e)
	}
	if len(e.Schedules) != 2 || len(e.ScheduleBackoffs) != 2 || e.PollPriority != 7 {
		t.Fatalf("schedules=%d backoffs=%d priority=%d", len(e.Schedules), len(e.ScheduleBackoffs), e.PollPriority)
	}
	// The schedule is evaluated in the instance's time zone: 09:00 New York.
	next := e.Schedules[0].Next(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC))
	if next.UTC().Hour() != 14 {
		t.Fatalf("09:00 America/New_York fires at %s UTC, want 14:00", next.UTC())
	}
	if got := strings.Join(e.Signals, ","); !strings.Contains(got, "deploy") || len(e.Signals) != 2 {
		t.Fatalf("signals = %v", e.Signals)
	}
	if e.PollFallbackCause == "" || e.DisabledReason != "" {
		t.Fatalf("webhook+schedule fallback cause %q, disabled %q", e.PollFallbackCause, e.DisabledReason)
	}
}

func TestBuildDisablesWhatItCannotServeWithANamedReason(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerBacklogItem})
	if got := build(t, cfg, set)[0].DisabledReason; got != BacklogPendingReason {
		t.Fatalf("backlog workflow disabled reason = %q", got)
	}
	// A disabled backlog trigger is not served either way, so it is not blocking.
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerBacklogItem, Enabled: off()}, apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"})
	if got := build(t, cfg, set)[0].DisabledReason; got != "" {
		t.Fatalf("disabled backlog trigger blocked the workflow: %q", got)
	}
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Workflows[0].Spec.Enabled = off()
	if got := build(t, cfg, set)[0].DisabledReason; !strings.Contains(got, "spec.enabled=false") {
		t.Fatalf("disabled workflow reason = %q", got)
	}
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Gaggles[0].Spec.Enabled = off()
	if got := build(t, cfg, set)[0].DisabledReason; !strings.Contains(got, `gaggle "g" is disabled`) {
		t.Fatalf("disabled gaggle reason = %q", got)
	}
}

func TestBuildSkipsDisabledTriggersButNeverManual(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(
		apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *", Enabled: off()},
		apiv1.Trigger{Type: apiv1.TriggerSignal, Signal: "x", Enabled: off()},
		apiv1.Trigger{Type: apiv1.TriggerManual, Enabled: off()},
	)
	if e := build(t, cfg, set)[0]; len(e.Schedules) != 0 || len(e.Signals) != 0 {
		t.Fatalf("disabled triggers registered: %+v", e)
	}
}

func TestBuildRefusesWhatWouldAdmitUnpinnedOrUnstartableWork(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	for name, in := range map[string]Input{
		"no generation":  {Config: cfg, Set: set, Pins: pins(set), Starter: starters},
		"no pins":        {Generation: "g", Config: cfg, Set: set, Starter: starters},
		"half a pin":     {Generation: "g", Config: cfg, Set: set, Starter: starters, Pins: map[string]cloudconfig.Pins{"g/wf": {WorkflowDigest: "w"}}},
		"unknown gaggle": {Generation: "g", Config: cfg, Set: func() *instance.ConfigSet { _, s := fixture(); s.Workflows[0].Spec.Gaggle = "missing"; return s }(), Pins: pins(set), Starter: starters},
		"bad time zone":  {Generation: "g", Config: &instance.Config{Timezone: "Not/AZone"}, Set: set, Pins: pins(set), Starter: starters},
	} {
		if entries, err := Build(in); err == nil {
			t.Errorf("%s: built %d entries", name, len(entries))
		}
	}
}

func TestAssessNamesEveryUnparseableTriggerAndValidateFlattensIt(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "not a cron"})
	findings := Assess(cfg, set)["g/wf"]
	if len(findings) != 1 || findings[0].Code != CodeTrigger || findings[0].Path != "/spec/triggers" || findings[0].Unblock == "" {
		t.Fatalf("findings = %+v, want one TRIGGER001 naming the workflow's triggers", findings)
	}
	err := Validate(cfg, set)
	if err == nil || !strings.Contains(err.Error(), "g/wf") {
		t.Fatalf("Validate = %v, want an error naming the workflow", err)
	}
	if _, buildErr := Build(Input{Generation: "g", Config: cfg, Set: set, Pins: pins(set), Starter: starters}); buildErr == nil {
		t.Fatal("Build accepted what Assess refuses")
	}
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "not a cron", Enabled: off()})
	if got := Assess(cfg, set); len(got) != 0 {
		t.Fatalf("a disabled trigger is never parsed: %v", got)
	}
	if err := Validate(nil, set); err == nil {
		t.Fatal("Validate accepted a missing configuration")
	}
	// An unloadable instance time zone blocks every workflow, because Build cannot
	// construct any entry without it.
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	cfg.Timezone = "Not/AZone"
	if got := Assess(cfg, set); len(got["g/wf"]) != 1 {
		t.Fatalf("bad time zone findings = %v", got)
	}
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	if got := Assess(cfg, set); len(got) != 0 {
		t.Fatalf("a clean workflow was blocked: %v", got)
	}
}

func TestBuildIsOrderedByGaggleAndWorkflow(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	set.Gaggles = append(set.Gaggles, apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
	for _, name := range []string{"z", "b"} {
		set.Workflows = append(set.Workflows, apiv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: apiv1.WorkflowSpec{Gaggle: "a"}})
	}
	var order []string
	for _, e := range build(t, cfg, set) {
		order = append(order, e.Gaggle+"/"+e.Workflow)
	}
	if strings.Join(order, " ") != "a/b a/z g/wf" {
		t.Fatalf("order = %v", order)
	}
}

func TestPackageClosureExcludesTheRunner(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/goobers/goobers/internal/cloud/schedentries").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "github.com/goobers/goobers/internal/runner" {
			t.Fatalf("internal/cloud/schedentries depends on %s", dep)
		}
	}
}

func startingWith(set *instance.ConfigSet, command ...string) {
	set.Workflows[0].Spec.Start = "first"
	set.Workflows[0].Spec.Tasks = []apiv1.Task{{Name: "first", Type: apiv1.TaskDeterministic, Goal: "g", Run: &apiv1.DeterministicRun{Command: command}}}
}

// TestBuildRefusesWorkflowsWhoseDemandCountersDoNotExistYet: the local daemon
// sizes scheduled remediation to eligible work and refills a desired occupancy;
// the cloud has neither counter until L1d, and must say so instead of firing
// unsized runs every tick or never refilling.
func TestBuildRefusesWorkflowsWhoseDemandCountersDoNotExistYet(t *testing.T) {
	t.Parallel()
	repos := []instance.RepoRef{{Provider: "github", Owner: "o", Name: "r"}}
	schedule := apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}

	cfg, set := fixture(schedule)
	cfg.Repos = repos
	startingWith(set, "goobers", "update-behind-pr")
	if got := build(t, cfg, set)[0].DisabledReason; got != ScheduleDemandPendingReason {
		t.Fatalf("scheduled remediation workflow reason = %q", got)
	}
	// Each condition the local daemon requires must be present.
	for name, mutate := range map[string]func(*instance.Config, *instance.ConfigSet){
		"no configured repos":   func(c *instance.Config, _ *instance.ConfigSet) { c.Repos = nil },
		"no schedule":           func(_ *instance.Config, s *instance.ConfigSet) { s.Workflows[0].Spec.Triggers = nil },
		"different first stage": func(_ *instance.Config, s *instance.ConfigSet) { startingWith(s, "goobers", "other") },
		"azure devops (unsized)": func(c *instance.Config, s *instance.ConfigSet) {
			s.Gaggles[0].Spec.Project.Provider = apiv1.ProviderADO
			c.Repos = []instance.RepoRef{{Provider: "ado", Owner: "o", Name: "r"}}
		},
		"disabled schedule": func(_ *instance.Config, s *instance.ConfigSet) { s.Workflows[0].Spec.Triggers[0].Enabled = off() },
	} {
		cfg, set := fixture(schedule)
		cfg.Repos = repos
		startingWith(set, "goobers", "update-behind-pr")
		mutate(cfg, set)
		if got := build(t, cfg, set)[0].DisabledReason; got != "" {
			t.Errorf("%s: refused with %q, local builds no counter here", name, got)
		}
	}

	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	cfg.Repos = repos
	set.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 2
	startingWith(set, "goobers", "backlog-query")
	if got := build(t, cfg, set)[0].DisabledReason; got != RefillPendingReason {
		t.Fatalf("desired-occupancy workflow reason = %q", got)
	}
	for name, mutate := range map[string]func(*instance.Config, *instance.ConfigSet){
		"no desired occupancy": func(_ *instance.Config, s *instance.ConfigSet) {
			s.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 0
		},
		"no configured repos":   func(c *instance.Config, _ *instance.ConfigSet) { c.Repos = nil },
		"different first stage": func(_ *instance.Config, s *instance.ConfigSet) { startingWith(s, "goobers", "other") },
	} {
		cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
		cfg.Repos = repos
		set.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 2
		startingWith(set, "goobers", "backlog-query")
		mutate(cfg, set)
		if got := build(t, cfg, set)[0].DisabledReason; got != "" {
			t.Errorf("%s: refused with %q, local builds no counter here", name, got)
		}
	}

	// A backlog-item trigger already polls and fans out, so it reports only the
	// backlog reason, never a second refill poll.
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerBacklogItem})
	cfg.Repos = repos
	set.Workflows[0].Spec.Readiness.DesiredConcurrentRuns = 2
	startingWith(set, "goobers", "backlog-query")
	if got := build(t, cfg, set)[0].DisabledReason; got != BacklogPendingReason {
		t.Fatalf("backlog workflow reason = %q", got)
	}
}

func TestSourcesReportAFailedUnpinAndStillReleasesOnSuccess(t *testing.T) {
	t.Parallel()
	var reported []error
	released := 0
	sources, err := NewSources(Deps{
		Queue:       nopQueue{},
		Generations: failingRelease{err: errors.New("unpin failed"), count: &released},
		Build: func(context.Context, startintent.Target) (startintent.Prepared, error) {
			return startintent.Prepared{}, nil
		},
		Report: func(err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := sources.Acquire(t.Context(), startintent.Target{ConfigGeneration: "sha256:gen"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if released != 1 || len(reported) != 1 || !strings.Contains(reported[0].Error(), "sha256:gen") {
		t.Fatalf("released=%d reported=%v, want one release attempt and one report naming the generation", released, reported)
	}
	if _, err := NewSources(Deps{}); err == nil {
		t.Fatal("NewSources accepted no dependencies")
	}
}

type failingRelease struct {
	err   error
	count *int
}

func (f failingRelease) Acquire(context.Context, string) (childworkflow.Generation, error) {
	return childworkflow.Generation{Release: func() error { *f.count++; return f.err }}, nil
}

type nopQueue struct {
	triggerqueue.SchedulerPolicyStore
}

type fakeCounter struct{}

func (fakeCounter) EligibleCount(context.Context) (int, error) { return 1, nil }

func TestSuppliedCountersClearOnlyTheirOwnPendingReason(t *testing.T) {
	t.Parallel()
	repos := []instance.RepoRef{{Provider: "github", Owner: "o", Name: "r"}}
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerBacklogItem}, apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"})
	cfg.Repos = repos
	startingWith(set, "goobers", "update-behind-pr")
	entry := func(c Counters) localscheduler.WorkflowEntry {
		entries, err := Build(Input{Generation: "g", Config: cfg, Set: set, Pins: pins(set), Starter: starters, Counters: func(localscheduler.WorkflowIdentity) Counters { return c }})
		if err != nil {
			t.Fatal(err)
		}
		return entries[0]
	}
	if got := entry(Counters{}).DisabledReason; got != BacklogPendingReason+"; "+ScheduleDemandPendingReason {
		t.Fatalf("no counters: %q", got)
	}
	if got := entry(Counters{Backlog: fakeCounter{}}).DisabledReason; got != ScheduleDemandPendingReason {
		t.Fatalf("backlog counter only: %q", got)
	}
	full := entry(Counters{Backlog: fakeCounter{}, ScheduleDemand: fakeCounter{}})
	if full.DisabledReason != "" || full.BacklogCounter == nil || full.ScheduleDemandCounter == nil || full.RefillDemandCounter != nil {
		t.Fatalf("with counters: reason %q, backlog=%v scheduleDemand=%v refill=%v", full.DisabledReason, full.BacklogCounter != nil, full.ScheduleDemandCounter != nil, full.RefillDemandCounter != nil)
	}
}

func TestMissingStartersYieldAnEntryThatRefusesToStart(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerManual})
	for name, factory := range map[string]func(localscheduler.WorkflowIdentity) localscheduler.Starter{
		"nil factory": nil,
		"nil result":  func(localscheduler.WorkflowIdentity) localscheduler.Starter { return nil },
	} {
		entries, err := Build(Input{Generation: "g", Config: cfg, Set: set, Pins: pins(set), Starter: factory})
		if err != nil || len(entries) != 1 || entries[0].Starter == nil {
			t.Fatalf("%s: %v %+v", name, err, entries)
		}
		if _, err := entries[0].Starter.Start(t.Context(), localscheduler.StartRequest{}); !errors.Is(err, ErrNoStarter) {
			t.Fatalf("%s: start = %v, want ErrNoStarter", name, err)
		}
	}
}

// TestScheduleDemandExemptionUsesTheConfiguredRepositoryProvider: a gaggle that
// omits its provider is bound to azure devops by the configured repository, so
// the local daemon builds no schedule counter there and neither may the cloud
// refuse it; conversely a configured GitHub repository overrides a stale ado ref.
func TestScheduleDemandExemptionUsesTheConfiguredRepositoryProvider(t *testing.T) {
	t.Parallel()
	schedule := apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}
	reason := func(project apiv1.RepoRef, configured instance.RepoRef) string {
		cfg, set := fixture(schedule)
		cfg.Repos = []instance.RepoRef{configured}
		set.Gaggles[0].Spec.Project = project
		startingWith(set, "goobers", "update-behind-pr")
		return build(t, cfg, set)[0].DisabledReason
	}
	omitted := apiv1.RepoRef{Owner: "org", Project: "proj", Name: "repo"}
	if got := reason(omitted, instance.RepoRef{Provider: "ado", Owner: "org", Project: "proj", Name: "repo"}); got != "" {
		t.Fatalf("omitted provider bound to ado by cfg.Repos was refused: %q", got)
	}
	if got := reason(omitted, instance.RepoRef{Provider: "github", Owner: "org", Project: "proj", Name: "repo"}); got != ScheduleDemandPendingReason {
		t.Fatalf("omitted provider bound to github by cfg.Repos: %q", got)
	}
	if got := reason(apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "o", Name: "r"}, instance.RepoRef{Provider: "ado", Owner: "o", Project: "p", Name: "other"}); got != ScheduleDemandPendingReason {
		t.Fatalf("an unrelated configured repo changed the provider: %q", got)
	}
}
