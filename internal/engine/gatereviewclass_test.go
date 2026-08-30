package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/temporal"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

// gatereviewclass_test.go covers the two post-merge defects filed against
// #3858: the kit-fetch classification gap (#3888) and the shared verdict
// validator's concurrency contract (#3887).

// #3888. The two kit-FETCH codes are surrendered from before the pod holds a
// kit, so the pod cannot know it was dispatched as a review and stamps them
// Retryable=false. The engine knows (input.Review) and classes them as
// infrastructure faults regardless — a reviewer that never started says
// nothing about the change under review, and the gate has no branch for it.
// Modeled on cmd/goobers's TestReviewPodKeepsTheHarnessInfrastructureClass,
// which pins the same split on the pod side.
func TestDispatchStageReviewKitFetchFailuresAreInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		message   string
		retryable bool
	}{
		{
			name:    "no kit digest stamped on the pod",
			code:    dispatcher.CodeAgenticKitMissing,
			message: "no agentic kit digest was stamped on this pod",
		},
		{
			name:    "the blob plane would not serve the kit",
			code:    dispatcher.CodeAgenticKitUnavailable,
			message: "fetch kit sha256:abc: dial tcp: connection refused",
		},
		{
			name:      "a kit-fetch code the pod did mark retryable stays infra",
			code:      dispatcher.CodeAgenticKitUnavailable,
			message:   "fetch kit sha256:abc: 503",
			retryable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := surrenderStore(t)
			putSurrendered(t, store, "run-kit", "review", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: tc.code, Message: tc.message, Retryable: tc.retryable},
			}})
			a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}
			input := dispatchInput("run-kit", "review", 1)
			input.Review = true

			_, err := a.DispatchStage(context.Background(), input)
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) {
				t.Fatalf("error = %v, want a typed application error", err)
			}
			if appErr.Type() != FailureTypeInfrastructure {
				t.Fatalf("failure type = %q (%v), want %q: a reviewer that never got its kit is a substrate fault, not a verdict", appErr.Type(), err, FailureTypeInfrastructure)
			}
			if !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want it to name the pod's own code and message", err)
			}
		})
	}
}

// The classification is REVIEW-only. A task attempt's surrendered failure is
// a business outcome the definition routes on, whatever its code: the same
// kit-fetch codes must still project as a ResultFailure envelope with no
// error at all, exactly as before #3888.
func TestDispatchStageTaskKitFetchFailureStaysABusinessOutcome(t *testing.T) {
	for _, code := range []string{dispatcher.CodeAgenticKitMissing, dispatcher.CodeAgenticKitUnavailable} {
		t.Run(code, func(t *testing.T) {
			store := surrenderStore(t)
			putSurrendered(t, store, "run-task-kit", "implement", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: code, Message: "kit fetch failed", Retryable: false},
			}})
			a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}

			result, err := a.DispatchStage(context.Background(), dispatchInput("run-task-kit", "implement", 1))
			if err != nil {
				t.Fatalf("DispatchStage = %v, want the surrendered failure projected as the stage's own outcome", err)
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != code {
				t.Fatalf("result = %+v, want the pod's ResultFailure envelope verbatim", result)
			}
		})
	}
}

// A review failure that is neither retryable nor a kit-fetch code keeps its
// policy class: the widened arm must not swallow the harness's own outcome.
func TestDispatchStageReviewHarnessFailureStaysPolicyClassed(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "run-policy", "review", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: "agentic_review_failed", Message: "harness refused the prompt", Retryable: false},
	}})
	a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}
	input := dispatchInput("run-policy", "review", 1)
	input.Review = true

	_, err := a.DispatchStage(context.Background(), input)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != FailureTypeStage {
		t.Fatalf("error = %v, want a %s-classed application error", err, FailureTypeStage)
	}
}

// #3887. verdictValidator is one process-wide validator read by every
// concurrent placed-gate review the worker runs, and the worker uses the
// Temporal SDK's default activity concurrency — so two gates surrendering
// verdicts at once reach it together on a cold cache. Run under -race: before
// the fix this reported races inside jsonschema.Compiler and panicked with
// "assignment to entry in nil map".
func TestConcurrentSurrenderedVerdictValidation(t *testing.T) {
	const goroutines = 32
	verdicts := []apiv1.Verdict{
		{Decision: apiv1.VerdictPass, Summary: "looks good"},
		{Decision: apiv1.VerdictNeedsChanges, Summary: "one more pass", Findings: []apiv1.Finding{{Severity: "error", Message: "missing test"}}},
		{Decision: apiv1.VerdictFail, Summary: "wrong change"},
	}

	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := validateSurrenderedVerdict(verdicts[i%len(verdicts)]); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent validateSurrenderedVerdict: %v", err)
	}

	// Fail-closed behaviour is unchanged under the same concurrency: an empty
	// decision and a decision outside the schema are still refused.
	var refusals sync.WaitGroup
	bad := make(chan error, 2*goroutines)
	for i := 0; i < goroutines; i++ {
		refusals.Add(1)
		go func(i int) {
			defer refusals.Done()
			if err := validateSurrenderedVerdict(apiv1.Verdict{Summary: "forgot to decide"}); err == nil {
				bad <- errors.New("an empty decision validated clean")
			}
			if err := validateSurrenderedVerdict(apiv1.Verdict{Decision: "maybe"}); err == nil {
				bad <- errors.New("a decision outside the verdict schema validated clean")
			}
		}(i)
	}
	refusals.Wait()
	close(bad)
	for err := range bad {
		t.Fatal(err)
	}
}
