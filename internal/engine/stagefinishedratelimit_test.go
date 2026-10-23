package engine

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func rateLimitedResult(reset string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Error:   &apiv1.ErrorInfo{Code: providers.ErrorCodeRateLimited, Message: "rate limited"},
		Outputs: map[string]any{executor.OutputRateLimitReset: reset},
	}
}

// TestStageFinishedKeepsTheRateLimitResetAcrossTheToleratedDiscard.
//
// A continueOnError'd failure has its outputs discarded, exactly as the local
// runner discards them, so the two drivers' journals match. That discard would
// take the rateLimitReset with it — and the local runner does not care,
// because its #614 notification fires at the TOP of the ResultFailure arm,
// BEFORE the continueOnError break, precisely so the scheduler learns about an
// exhausted provider window regardless of what this run then does with the
// failure.
//
// An engine run's walk has no access to the daemon's ProviderQuotaState; its
// only channel into that process is the journal. So the reset survives on the
// event's Runner map, which journal.ConformanceView does not project — the
// parity comparison is unaffected, and the daemon's live observer can still
// park the scheduler.
func TestStageFinishedKeepsTheRateLimitResetAcrossTheToleratedDiscard(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ev := stageFinishedEvent("poll-ci", 1, journal.AttemptPolicy, rateLimitedResult(reset), true)

	if len(ev.Outputs) != 0 {
		t.Fatalf("Outputs = %v, want them discarded exactly as the local runner discards a tolerated failure's", ev.Outputs)
	}
	got, ok := ev.Runner[executor.OutputRateLimitReset].(string)
	if !ok || got != reset {
		t.Fatalf("runner[%s] = %v, want the reset %q; without it the scheduler keeps admitting runs into an exhausted window", executor.OutputRateLimitReset, ev.Runner[executor.OutputRateLimitReset], reset)
	}
	// Not conformance-normative, so the runner's journal for the same outcome
	// still compares equal.
	if len(journal.ConformanceView([]journal.Event{ev})) != len(journal.ConformanceView([]journal.Event{{
		Type: journal.EventStageFinished, Stage: "poll-ci", Attempt: 1,
		AttemptClass: journal.AttemptPolicy, Status: string(apiv1.ResultFailure),
		Error: &journal.ErrorDetail{Code: providers.ErrorCodeRateLimited, Message: "rate limited"},
	}})) {
		t.Error("the annotation changed the conformance view; the two drivers' journals would no longer compare equal")
	}
}

// TestStageFinishedDoesNotAnnotateWhatItDidNotDiscard: an untolerated
// rate-limited failure keeps its outputs, so duplicating the reset onto the
// Runner map would be two sources of truth for one fact.
func TestStageFinishedDoesNotAnnotateWhatItDidNotDiscard(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	ev := stageFinishedEvent("poll-ci", 1, journal.AttemptPolicy, rateLimitedResult(reset), false)
	if len(ev.Outputs) == 0 {
		t.Fatal("an untolerated failure's outputs were discarded")
	}
	if _, dup := ev.Runner[executor.OutputRateLimitReset]; dup {
		t.Error("the reset was duplicated onto the Runner map for an outcome that still carries its outputs")
	}

	// A tolerated failure that is not rate-limited carries nothing for the
	// observer to act on, and must not grow an empty annotation.
	plain := apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Error:   &apiv1.ErrorInfo{Code: "some_other_failure"},
		Outputs: map[string]any{"whatever": "value"},
	}
	if ev := stageFinishedEvent("poll-ci", 1, journal.AttemptPolicy, plain, true); len(ev.Runner) != 0 {
		t.Errorf("runner = %v, want nothing for a tolerated failure that is not rate-limited", ev.Runner)
	}

	// And a rate-limited failure whose reset could not be recovered must not
	// record a zero value, which would park the scheduler forever.
	noReset := rateLimitedResult("")
	delete(noReset.Outputs, executor.OutputRateLimitReset)
	if ev := stageFinishedEvent("poll-ci", 1, journal.AttemptPolicy, noReset, true); len(ev.Runner) != 0 {
		t.Errorf("runner = %v, want nothing when there is no reset to carry", ev.Runner)
	}
}
