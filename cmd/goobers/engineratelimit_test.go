package main

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestEngineRateLimitObserverRecordsExhaustionMidRun is the hazard the
// observer exists to close.
//
// #614's RateLimited handler fires MID-RUN on the local runner
// (notifyRateLimited, at the failing stage), so the scheduler stops admitting
// provider-dependent runs the moment the provider says it is out of quota. An
// engine run's walk happens on a Temporal worker with no access to this
// daemon's ProviderQuotaState; its only channel into this process is the
// journal event stream. Without this observer an engine lane would keep the
// scheduler admitting runs for the whole remaining duration of a run that has
// already hit the limit — every one of them guaranteed to fail the same way.
//
// The observer must record the reset the STAGE declared, not a guess: parking
// for the wrong window either wastes quota or wedges the scheduler.
func TestEngineRateLimitObserverRecordsExhaustionMidRun(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	observe := engineRateLimitObserver(quota)
	if observe == nil {
		t.Fatal("no observer built for a live quota state")
	}
	resetAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	observe("run-1", journal.Event{
		Type:  journal.EventStageFinished,
		Stage: "implement",
		Error: &journal.ErrorDetail{Code: providers.ErrorCodeRateLimited, Message: "rate limited"},
		Outputs: map[string]any{
			executor.OutputRateLimitReset: resetAt.Format(time.RFC3339),
		},
	})

	until, exhausted := quota.Exhausted(time.Now())
	if !exhausted {
		t.Fatal("the scheduler's quota state was not parked; it would keep admitting runs that are all going to fail the same way")
	}
	if !until.Equal(resetAt) {
		t.Errorf("parked until %v, want the reset the stage declared (%v)", until, resetAt)
	}
}

// TestEngineRateLimitObserverIgnoresEverythingElse: the observer runs inside
// the live-journal apply path, under the run's lock, for EVERY append. It must
// be cheap and it must be precise — parking the scheduler on an ordinary stage
// failure would be a self-inflicted outage.
func TestEngineRateLimitObserverIgnoresEverythingElse(t *testing.T) {
	resetAt := time.Now().Add(30 * time.Minute).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		ev   journal.Event
	}{
		{"a successful stage", journal.Event{Type: journal.EventStageFinished, Stage: "implement"}},
		{
			"a stage that failed for another reason",
			journal.Event{
				Type:    journal.EventStageFinished,
				Error:   &journal.ErrorDetail{Code: executor.CIPollProviderErrorCode, Message: "502"},
				Outputs: map[string]any{executor.OutputRateLimitReset: resetAt},
			},
		},
		{
			// A rate limit with no reset header carries nothing actionable.
			// internal/runner's taskOutcome skips notifyRateLimited entirely
			// for it, and recording a zero reset here would park the
			// scheduler on a window that already elapsed — or, worse, be read
			// as "forever" by a consumer that does not expect the zero time.
			"a rate limit with no declared reset",
			journal.Event{
				Type:  journal.EventStageFinished,
				Error: &journal.ErrorDetail{Code: providers.ErrorCodeRateLimited},
			},
		},
		{
			"a rate limit whose reset is unparseable",
			journal.Event{
				Type:    journal.EventStageFinished,
				Error:   &journal.ErrorDetail{Code: providers.ErrorCodeRateLimited},
				Outputs: map[string]any{executor.OutputRateLimitReset: "not a timestamp"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quota := localscheduler.NewProviderQuotaState()
			engineRateLimitObserver(quota)("run-1", tc.ev)
			if _, exhausted := quota.Exhausted(time.Now()); exhausted {
				t.Fatalf("%s parked the scheduler's provider quota", tc.name)
			}
		})
	}
}

// TestEngineRateLimitOptionIsAbsentWithoutQuotaState: an instance with no
// quota state has nothing to record into, and the writer must not be given a
// no-op observer that runs on every append for nothing.
func TestEngineRateLimitOptionIsAbsentWithoutQuotaState(t *testing.T) {
	if engineRateLimitOption(nil) != nil {
		t.Error("a live-journal option was built with no quota state to record into")
	}
	if engineRateLimitObserver(nil) != nil {
		t.Error("an observer was built with no quota state to record into")
	}
}

// TestEngineRateLimitObserverRecordsAToleratedRateLimit is the
// continueOnError hole.
//
// runJournal.stageFinished discards a tolerated failure's outputs — the local
// runner discards them too, so the two journals match — which would take the
// rateLimitReset output with it. The local runner does not care: its #614
// notification fires at the TOP of the ResultFailure arm, BEFORE the
// continueOnError break, precisely so the scheduler learns about an exhausted
// window regardless of what this run then does with the failure. An engine
// run's only channel to this daemon's ProviderQuotaState is the journal, so
// the reset is carried on the event's Runner map (not conformance-normative)
// and the observer must read it there.
//
// Without this, a continueOnError'd provider-polling stage hitting the limit
// on an engine lane leaves the quota untouched, and the scheduler keeps
// admitting provider-dependent runs into an exhausted window.
func TestEngineRateLimitObserverRecordsAToleratedRateLimit(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	observe := engineRateLimitObserver(quota)
	resetAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	observe("run-1", journal.Event{
		Type:   journal.EventStageFinished,
		Stage:  "poll-ci",
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: providers.ErrorCodeRateLimited, Message: "rate limited"},
		// Outputs discarded by the tolerated-failure arm.
		Runner: map[string]any{executor.OutputRateLimitReset: resetAt.Format(time.RFC3339)},
	})

	got, exhausted := quota.Exhausted(time.Now())
	if !exhausted {
		t.Fatal("a tolerated rate-limited stage left the provider quota untouched; the scheduler would keep admitting runs into an exhausted window")
	}
	if !got.Equal(resetAt) {
		t.Errorf("reset = %s, want %s", got, resetAt)
	}
}

// TestEngineRateLimitObserverIgnoresARunnerMapWithoutAReset: the Runner map
// is the daemon's own annotation channel and carries plenty of other facts.
// Only a parseable reset may park the scheduler.
func TestEngineRateLimitObserverIgnoresARunnerMapWithoutAReset(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	observe := engineRateLimitObserver(quota)

	for _, runnerFacts := range []map[string]any{
		{"driver": "engine"},
		{executor.OutputRateLimitReset: "not-a-timestamp"},
		{executor.OutputRateLimitReset: 42},
	} {
		observe("run-1", journal.Event{
			Type:   journal.EventStageFinished,
			Error:  &journal.ErrorDetail{Code: providers.ErrorCodeRateLimited},
			Runner: runnerFacts,
		})
	}
	if _, exhausted := quota.Exhausted(time.Now()); exhausted {
		t.Error("the scheduler was parked on a rate limit whose reset could not be recovered")
	}
}
