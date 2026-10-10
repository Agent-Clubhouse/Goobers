package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/readservice"
)

func TestStatusBackpropOverrideEnrollsOffWorkflowsInShadow(t *testing.T) {
	workflows := []apiv1.Workflow{
		{Spec: apiv1.WorkflowSpec{Gaggle: "observed"}},
		{Spec: apiv1.WorkflowSpec{Gaggle: "observed", Backprop: &apiv1.BackpropConfig{Mode: apiv1.BackpropModeOff, Version: "v1"}}},
		{Spec: apiv1.WorkflowSpec{Gaggle: "plain"}},
	}
	for i, name := range []string{"unconfigured", "opted-out", "elsewhere"} {
		workflows[i].Name = name
	}
	base := func([]apiv1.Workflow, []runSummary, readservice.SchedulerStatus, time.Time) (statusFleetSummary, error) {
		summary := statusFleetSummary{}
		for _, workflow := range workflows {
			summary.Workflows = append(summary.Workflows, statusWorkflowSummary{
				Workflow: workflow.Name, Gaggle: workflow.Spec.Gaggle,
				Backprop: readservice.WorkflowBackpropFor(workflow.Spec.Backprop, nil),
			})
		}
		return summary, nil
	}
	gaggles := []apiv1.Gaggle{{Spec: apiv1.GaggleSpec{Backprop: &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeShadow}}}}
	gaggles[0].Name = "observed"
	summary, err := withStatusBackpropOverrides(base, gaggles)(workflows, nil, readservice.SchedulerStatus{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"unconfigured": "shadow", "opted-out": "off", "elsewhere": "off"}
	for _, workflow := range summary.Workflows {
		mode := workflow.Backprop.Mode
		if mode == "" {
			mode = "off"
		}
		if mode != want[workflow.Workflow] {
			t.Fatalf("%s backprop = %+v, want mode %s", workflow.Workflow, workflow.Backprop, want[workflow.Workflow])
		}
	}
}

func TestTelemetryShadowReportsEmptyInstanceReadOnly(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runTelemetryShadow([]string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no shadow-mode runs found") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	stdout.Reset()
	if code := runTelemetryShadow([]string{"--json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"schema": "goobers.dev/backprop/shadow-report/v1"`) {
		t.Fatalf("json stdout = %q", stdout.String())
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("instance root entries = %v, %v; want shadow report to write nothing", entries, err)
	}
}
