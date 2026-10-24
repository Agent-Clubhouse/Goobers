package schedentries

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
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
		"no starter":     {Generation: "g", Config: cfg, Set: set, Pins: pins(set)},
		"no pins":        {Generation: "g", Config: cfg, Set: set, Starter: starters},
		"half a pin":     {Generation: "g", Config: cfg, Set: set, Starter: starters, Pins: map[string]cloudconfig.Pins{"g/wf": {WorkflowDigest: "w"}}},
		"nil starter":    {Generation: "g", Config: cfg, Set: set, Pins: pins(set), Starter: func(localscheduler.WorkflowIdentity) localscheduler.Starter { return nil }},
		"unknown gaggle": {Generation: "g", Config: cfg, Set: func() *instance.ConfigSet { _, s := fixture(); s.Workflows[0].Spec.Gaggle = "missing"; return s }(), Pins: pins(set), Starter: starters},
		"bad time zone":  {Generation: "g", Config: &instance.Config{Timezone: "Not/AZone"}, Set: set, Pins: pins(set), Starter: starters},
	} {
		if entries, err := Build(in); err == nil {
			t.Errorf("%s: built %d entries", name, len(entries))
		}
	}
}

func TestValidateNamesTheUnparseableTrigger(t *testing.T) {
	t.Parallel()
	cfg, set := fixture(apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "not a cron"})
	err := Validate(cfg, set)
	if err == nil || !strings.Contains(err.Error(), `workflow "wf"`) {
		t.Fatalf("Validate = %v, want an error naming the workflow", err)
	}
	if _, buildErr := Build(Input{Generation: "g", Config: cfg, Set: set, Pins: pins(set), Starter: starters}); buildErr == nil {
		t.Fatal("Build accepted what Validate refuses")
	}
	cfg, set = fixture(apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "not a cron", Enabled: off()})
	if err := Validate(cfg, set); err != nil {
		t.Fatalf("a disabled trigger is never parsed: %v", err)
	}
	if err := Validate(nil, set); err == nil {
		t.Fatal("Validate accepted a missing configuration")
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
