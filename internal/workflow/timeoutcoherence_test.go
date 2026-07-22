package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
)

func TestStageTimeoutCoherence(t *testing.T) {
	tests := []struct {
		name       string
		task       apiv1.Task
		want       bool
		wantDetail []string
	}{
		{
			name: "issue 884 shape uses executor default",
			task: pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "30m",
			}),
			want:       true,
			wantDetail: []string{`task "queue-watch"`, "inputs.pollTimeoutSeconds", "30m0s", "effective stage timeout 10m0s", "before the stage writes a result"},
		},
		{
			name: "wait equal to stage timeout",
			task: pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "10m",
			}),
		},
		{
			name: "wait below legacy stage timeout",
			task: pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "25m",
				boundedwait.InputTimeout:     "30m",
			}),
		},
		{
			name: "canonical timeout overrides legacy input",
			task: withTimeout(pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "25m",
				boundedwait.InputTimeout:     "30m",
			}), 600),
			want:       true,
			wantDetail: []string{"25m0s", "effective stage timeout 10m0s"},
		},
		{
			name: "limits provide effective timeout",
			task: withLimits(pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "11m",
			}), 600),
			want: true,
		},
		{
			name: "canonical timeout overrides limits",
			task: withLimits(withTimeout(pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "20m",
			}), 600), 1800),
			want:       true,
			wantDetail: []string{"effective stage timeout 10m0s"},
		},
		{
			name: "merge queue clamp makes issue 884 shape safe",
			task: pollTask([]string{"goobers", "merge-queue-poll"}, map[string]string{
				boundedwait.InputPollTimeout: "30m",
			}),
		},
		{
			name: "merge queue default is clamped",
			task: pollTask([]string{"goobers", "merge-queue-poll"}, nil),
		},
		{
			name: "merge queue clamp still exceeds canonical timeout",
			task: withTimeout(pollTask([]string{"goobers", "merge-queue-poll"}, map[string]string{
				boundedwait.InputPollTimeout: "30m",
				boundedwait.InputTimeout:     "30m",
			}), 300),
			want:       true,
			wantDetail: []string{"27m0s", "clamped from 30m0s by merge-queue-poll", "effective stage timeout 5m0s"},
		},
		{
			name:       "merge queue default clamp uses executor default",
			task:       withTimeout(pollTask([]string{"goobers", "merge-queue-poll"}, nil), 300),
			want:       true,
			wantDetail: []string{"9m0s", "default 30m0s", "effective stage timeout 5m0s"},
		},
		{
			name: "ci poll clamp stays within canonical timeout",
			task: withTimeout(pollTask([]string{"goobers", "ci-poll"}, map[string]string{
				boundedwait.InputKind:        boundedwait.KindCIPoll,
				boundedwait.InputPollTimeout: "30m",
			}), 600),
		},
		{
			name: "dynamic wait is not statically diagnosed",
			task: withInputsFrom(pollTask([]string{"watch-queue"}, nil), map[string]string{
				boundedwait.InputPollTimeout: "upstreamWait",
			}),
		},
		{
			name: "dynamic legacy stage timeout is not statically diagnosed",
			task: withInputsFrom(pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputPollTimeout: "30m",
			}), map[string]string{
				boundedwait.InputTimeout: "upstreamTimeout",
			}),
		},
		{
			name: "unbounded command has no declared wait",
			task: pollTask([]string{"run-forever"}, nil),
		},
		{
			name: "unknown executor kind is not diagnosed",
			task: withTimeout(pollTask([]string{"watch-queue"}, map[string]string{
				boundedwait.InputKind:        "custom",
				boundedwait.InputPollTimeout: "30m",
			}), 600),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{Name: "wf", Spec: apiv1.WorkflowSpec{
				Start: tc.task.Name,
				Tasks: []apiv1.Task{tc.task},
			}}
			problems := CheckStageTimeoutCoherence(def)
			if tc.want && len(problems) != 1 {
				t.Fatalf("problems = %v, want one", problems)
			}
			if !tc.want && len(problems) != 0 {
				t.Fatalf("problems = %v, want none", problems)
			}
			if !tc.want {
				return
			}
			for _, detail := range tc.wantDetail {
				if !strings.Contains(problems[0], detail) {
					t.Errorf("problem %q missing %q", problems[0], detail)
				}
			}
		})
	}
}

func TestShippedWorkflowsHaveCoherentBoundedWaits(t *testing.T) {
	for _, root := range shippedWorkflowRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		var seen int
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			seen++
			path := filepath.Join(root, entry.Name())
			t.Run(filepath.Join(filepath.Base(filepath.Dir(root)), entry.Name()), func(t *testing.T) {
				for _, problem := range CheckStageTimeoutCoherence(loadWorkflowFile(t, path)) {
					t.Errorf("%s", problem)
				}
			})
		}
		if seen == 0 {
			t.Fatalf("no workflow yaml found under %s", root)
		}
	}
}

func pollTask(command []string, inputs map[string]string) apiv1.Task {
	return apiv1.Task{
		Name:   "queue-watch",
		Type:   apiv1.TaskDeterministic,
		Goal:   "Wait for a result.",
		Run:    &apiv1.DeterministicRun{Command: command},
		Inputs: inputs,
	}
}

func withTimeout(task apiv1.Task, seconds int32) apiv1.Task {
	task.TimeoutSeconds = seconds
	return task
}

func withLimits(task apiv1.Task, seconds int32) apiv1.Task {
	task.Limits = &apiv1.Limits{MaxDurationSeconds: seconds}
	return task
}

func withInputsFrom(task apiv1.Task, inputsFrom map[string]string) apiv1.Task {
	task.InputsFrom = inputsFrom
	return task
}
