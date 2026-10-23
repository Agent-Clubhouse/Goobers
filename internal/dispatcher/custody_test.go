package dispatcher

import (
	"context"
	"testing"
	"time"
)

func TestDispatchReattachAdoptsLivePodByUID(t *testing.T) {
	pods := &fakePodAPI{}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.sleep = func(context.Context, time.Duration) error { return nil }
	attempt := testAttempt()
	attempt.OwningWorkflowID = "run-workflow"
	pod, err := d.renderFor(t.Context(), attempt, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	var custody PodCustody
	ctx := WithPodCustodyObserver(t.Context(), func(observed PodCustody) { custody = observed })
	if err := d.createAttemptPod(ctx, pod, attempt, &Report{}); err != nil {
		t.Fatal(err)
	}
	report, err := d.AdoptPod(t.Context(), attempt, custody)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.created) != 1 || report.Pod != custody.Pod {
		t.Fatalf("adoption created replacement or lost custody: created=%v report=%+v custody=%+v", pods.created, report, custody)
	}

	custody.UID = "replacement"
	if _, err := d.AdoptPod(t.Context(), attempt, custody); err == nil {
		t.Fatal("changed pod UID was adopted")
	}
}
