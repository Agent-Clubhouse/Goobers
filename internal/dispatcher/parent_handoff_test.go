package dispatcher

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestParentHandoffRequiresExactStoppedWriters(t *testing.T) {
	for _, scenario := range []string{"stopped", "replaced", "phase-only", "name-only-api"} {
		t.Run(scenario, func(t *testing.T) {
			pods := &childPodFake{changeUID: scenario == "replaced", missingStatus: scenario == "phase-only"}
			var api PodAPI = pods
			if scenario == "name-only-api" {
				api = &pods.fakePodAPI
			}
			d, err := New(testConfig(), api, nil, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeClock{}
			d.now, d.sleep = clock.Now, clock.Sleep
			attempt := testAttempt()
			attempt.ChildWorkflowAccess = `{"endpoint":"https://control.example","bearerToken":"goobers-child.test"}`
			report, err := d.Dispatch(t.Context(), attempt, []RunnerSpec{linuxRunner()})
			if scenario == "stopped" {
				if err != nil || !report.ParentWritersStopped || pods.finalizedUID != "created-uid" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
			} else if report.ParentWritersStopped || pods.finalizedUID != "" {
				t.Fatalf("unproved parent acknowledged: %+v (%v)", report, err)
			}
		})
	}
}

func TestParentHandoffGracefulStopKeepsObjectUntilTermination(t *testing.T) {
	pods := &childPodFake{holdUntilStop: true}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := &fakeClock{}
	d.sleep = func(ctx context.Context, duration time.Duration) error {
		if pods.deletedUID == "" {
			cancel()
			return ctx.Err()
		}
		return clock.Sleep(ctx, duration)
	}
	attempt := testAttempt()
	attempt.ChildWorkflowAccess = `{"endpoint":"https://control.example","bearerToken":"goobers-child.test"}`
	report, _ := d.Dispatch(ctx, attempt, []RunnerSpec{linuxRunner()})
	if report.Phase != corev1.PodSucceeded || !report.ParentWritersStopped || pods.deletedUID != "created-uid" || pods.finalizedUID != "created-uid" {
		t.Fatalf("stop evidence=%+v", report)
	}
}
