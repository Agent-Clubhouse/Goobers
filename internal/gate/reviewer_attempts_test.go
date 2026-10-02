package gate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

type lifecycleReviewer struct {
	numbers []int32
	journal *lifecycleJournal
}

func (*lifecycleReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, nil
}
func (r *lifecycleReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.numbers = append(r.numbers, env.Attempt)
	if last := r.journal.events[len(r.journal.events)-1]; last.Type != journal.EventReviewerStarted || last.Attempt != int(env.Attempt) {
		panic("review dispatched before its durable start")
	}
	switch len(r.numbers) {
	case 1:
		return apiv1.Verdict{}, invoke.InfrastructureFailure(errors.New("transport lost"))
	case 2:
		return apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "Missing question"}, nil
	default:
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}
}

type lifecycleJournal struct {
	events    []journal.Event
	failStart bool
}

func (j *lifecycleJournal) Append(e journal.Event) error {
	if j.failStart && e.Type == journal.EventReviewerStarted {
		return errors.New("disk full")
	}
	j.events = append(j.events, e)
	return nil
}
func (*lifecycleJournal) RecordArtifact(string, []byte) (journal.Ref, error) {
	return journal.Ref{}, nil
}

func TestReviewerLifecycleNumbersAndClassifiesEachDispatch(t *testing.T) {
	j := &lifecycleJournal{}
	r := &lifecycleReviewer{journal: j}
	e := &Evaluator{Journal: j, Reviewer: &ReviewerEvaluator{Goober: r}, IsNeedsHumanTarget: func(target string) bool { return target == "implement" }}
	g := retryGate(&apiv1.RetryPolicy{MaxAttempts: 3})
	for visit := 0; visit < 2; visit++ {
		if _, err := e.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(r.numbers, []int32{1, 2, 3, 1}) {
		t.Fatalf("invocation attempts=%v", r.numbers)
	}
	var starts, ends []journal.Event
	for _, event := range j.events {
		if event.Type == journal.EventReviewerStarted {
			starts = append(starts, event)
		}
		if event.Type == journal.EventReviewerFinished {
			ends = append(ends, event)
		}
	}
	if len(starts) != 4 || len(ends) != 4 {
		t.Fatalf("starts=%v ends=%v", starts, ends)
	}
	want := []journal.AttemptClass{"", journal.AttemptInfra, journal.AttemptPolicy, ""}
	for i := range starts {
		if starts[i].AttemptClass != want[i] || ends[i].AttemptClass != want[i] {
			t.Fatalf("attempt %d lost class: %+v %+v", i, starts[i], ends[i])
		}
	}
	if ends[0].Runner["retryFailureClass"] != "infra" || ends[1].Error.Code != "verdict_invalid" {
		t.Fatalf("failure lineage=%+v", ends)
	}
}

func TestReviewerLifecycleContinuesInterruptedAttempt(t *testing.T) {
	j := &lifecycleJournal{}
	r := &lifecycleReviewer{journal: j, numbers: []int32{1, 2, 3}}
	e := &Evaluator{Journal: j, Reviewer: &ReviewerEvaluator{Goober: r}, ReviewerContinuation: func(string) (int, error) { return 4, nil }}
	if _, err := e.Evaluate(context.Background(), retryGate(nil), apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range j.events {
		if event.Type == journal.EventReviewerStarted && (event.Attempt != 5 || event.AttemptClass != journal.AttemptInfra) {
			t.Fatalf("recovery=%+v", event)
		}
	}
	if r.numbers[3] != 5 {
		t.Fatal(r.numbers)
	}
}

func TestReviewerLifecycleStartFailurePreventsInvocation(t *testing.T) {
	j := &lifecycleJournal{failStart: true}
	r := &lifecycleReviewer{journal: j}
	e := &Evaluator{Journal: j, Reviewer: &ReviewerEvaluator{Goober: r}}
	if _, err := e.Evaluate(context.Background(), retryGate(nil), apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false); err == nil {
		t.Fatal("missing journal failure")
	}
	if len(r.numbers) != 0 {
		t.Fatal("reviewer invoked without durable start")
	}
}

func TestCachedReviewerDoesNotInventAttempt(t *testing.T) {
	j := &lifecycleJournal{}
	r := &lifecycleReviewer{journal: j}
	e := &Evaluator{Journal: j, Reviewer: &ReviewerEvaluator{Goober: r}, CachedVerdict: &apiv1.Verdict{Decision: apiv1.VerdictPass}}
	if _, err := e.Evaluate(context.Background(), retryGate(nil), apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "", false); err != nil {
		t.Fatal(err)
	}
	for _, event := range j.events {
		if event.Type == journal.EventReviewerStarted {
			t.Fatal("cached verdict invented reviewer attempt")
		}
	}
}
