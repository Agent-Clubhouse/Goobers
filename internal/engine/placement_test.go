package engine

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Per-stage placement is what makes a single run span platforms: Temporal
// supports a task queue per ACTIVITY, so the workflow stays on one queue while
// individual stages are polled elsewhere.
func TestPlatformQueueSuffix(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		want string
	}{
		{"no capabilities inherits", nil, ""},
		{"unrelated capabilities inherit", []string{"python@3.12", "dotnet@8"}, ""},
		{"explicit linux inherits, it is the default", []string{"os=linux"}, ""},
		{"windows routes", []string{"os=windows"}, "windows"},
		{"windows among others routes", []string{"python@3.12", "os=windows"}, "windows"},
		{"darwin routes", []string{"os=darwin"}, "darwin"},
		{"empty os= is ignored rather than routing to a bare suffix", []string{"os="}, ""},
		{"first platform wins deterministically", []string{"os=windows", "os=darwin"}, "windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformQueueSuffix(tc.caps); got != tc.want {
				t.Fatalf("platformQueueSuffix(%v) = %q, want %q", tc.caps, got, tc.want)
			}
		})
	}
}

// An unroutable stage must fail rather than wait forever: Temporal's default
// ScheduleToStartTimeout is unlimited, which would make a missing Windows
// worker indistinguishable from a slow one.
func TestStageActivityOptionsBoundScheduleToStart(t *testing.T) {
	opts := stageActivityOptions(apiv1.Limits{}, "goobers-spike-windows")
	if opts.ScheduleToStartTimeout <= 0 {
		t.Fatal("ScheduleToStartTimeout must be bounded so an unroutable stage fails fast")
	}
	if opts.TaskQueue != "goobers-spike-windows" {
		t.Fatalf("TaskQueue = %q, want the routed queue", opts.TaskQueue)
	}
	if inherited := stageActivityOptions(apiv1.Limits{}, ""); inherited.TaskQueue != "" {
		t.Fatalf("empty queue must inherit the workflow's, got %q", inherited.TaskQueue)
	}
}
