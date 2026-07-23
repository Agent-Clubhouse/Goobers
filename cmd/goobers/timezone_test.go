package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// scheduleTriggeredWorkflowYAML is deterministicWorkflowYAML's sibling with a
// schedule trigger instead of a backlog-item one, so a test can inspect the
// resulting localscheduler.WorkflowEntry.Schedules directly.
const scheduleTriggeredWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "30 1 * * *"
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        command: ["true"]
`

// TestBuildSchedulerSetupWiresConfiguredTimezone is issue #137's timezone-
// wiring acceptance: a workflow's cron schedule must evaluate in
// instance.yaml's configured Timezone, not whatever zone the host process's
// own clock happens to be in. internal/localscheduler's own test suite
// already proves InLocation itself is DST-correct; this proves
// buildSchedulerSetup actually threads Config.Timezone into it, which is the
// part that was missing entirely before this fix (InLocation had zero
// production callers).
func TestBuildSchedulerSetupWiresConfiguredTimezone(t *testing.T) {
	if _, err := time.LoadLocation("America/New_York"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(scheduleTriggeredWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timezone = "America/New_York"
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())

	var sched interface {
		Next(time.Time) time.Time
	}
	for _, e := range setup.Entries {
		if e.Workflow == "default-implement" && len(e.Schedules) > 0 {
			sched = e.Schedules[0]
		}
	}
	if sched == nil {
		t.Fatal("expected default-implement's WorkflowEntry to have a non-empty Schedules")
	}

	// A bare UTC instant fed into the wired schedule must resolve "30 1 * * *"
	// against America/New_York's wall clock, not UTC's — if Timezone weren't
	// actually threaded through, this would compute the next 01:30 UTC
	// instead, which differs from 01:30 America/New_York by that zone's
	// offset (4 or 5 hours depending on DST).
	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	next := sched.Next(after)

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	inLoc := next.In(loc)
	if inLoc.Hour() != 1 || inLoc.Minute() != 30 {
		t.Fatalf("next fire = %v (America/New_York wall clock %02d:%02d), want 01:30 in that zone — Timezone config was not wired through", next, inLoc.Hour(), inLoc.Minute())
	}
}

// multiScheduleWorkflowYAML declares two schedule triggers on the same
// workflow — #341's fixture.
const multiScheduleWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "0 8 * * *"
    - type: schedule
      schedule: "0 14 * * *"
  start: local-ci
  tasks:
    - name: local-ci
      type: deterministic
      goal: run a no-op local command
      run:
        command: ["true"]
`

// TestBuildSchedulerSetupWiresAllScheduleTriggers is #341's regression:
// buildSchedulerSetup previously stopped at the first schedule-type trigger
// (a `break` inside the loop) — a second one compiled fine (once #142's
// compile-time reject was in place, it was actually a hard error; #341
// replaced that reject with real support) but the runtime scheduler would
// still only ever have wired the first. This proves both configured
// schedules land in WorkflowEntry.Schedules, not just one.
func TestBuildSchedulerSetupWiresAllScheduleTriggers(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(multiScheduleWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatalf("buildSchedulerSetup: %v", err)
	}
	defer setup.Shutdown(context.Background())

	var got []time.Time
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range setup.Entries {
		if e.Workflow != "default-implement" {
			continue
		}
		if len(e.Schedules) != 2 {
			t.Fatalf("Schedules = %d entries, want 2 (both configured triggers)", len(e.Schedules))
		}
		for _, sched := range e.Schedules {
			got = append(got, sched.Next(base))
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly one default-implement WorkflowEntry with 2 schedules, got %d schedule evaluations", len(got))
	}
	wantHours := map[int]bool{8: true, 14: true}
	for _, next := range got {
		if !wantHours[next.Hour()] {
			t.Errorf("unexpected next-fire hour %d (from %v) — want 8 or 14", next.Hour(), next)
		}
		delete(wantHours, next.Hour())
	}
	if len(wantHours) != 0 {
		t.Errorf("missing expected fire hours: %v", wantHours)
	}
}
