package engine

// RunResult.NoWork — the #233 short-circuit accounting, plan item E2 (#3874).
//
// The parity row (parity_row_runresult_nowork_test.go) proves the engine and
// the local runner agree about WHEN the flag is set. These tests pin the two
// things that row cannot reach: that adding the field to a persisted Temporal
// payload is replay-safe, and that the flag survives the trip through the
// daemon's Starter mapping into the scheduler's idle backoff — which is the
// only reason the flag exists at all.

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// A RunResult is a Temporal payload: it is persisted in workflow history and
// decoded again on replay, by a binary that may predate or postdate the field.
// Adding NoWork therefore has to be a no-op for both directions of that skew.
//
// This is the whole reason the field is `omitempty` rather than a plain bool,
// and it is the kind of decision that survives review and then quietly regresses
// when someone "tidies up" the struct tags.
func TestRunResultNoWorkIsReplaySafe(t *testing.T) {
	t.Run("a history written before the field decodes as false", func(t *testing.T) {
		// Verbatim shape of a completed result as it was persisted before E2.
		legacy := []byte(`{"status":"completed","finalState":"curate","steps":4}`)
		var got RunResult
		if err := json.Unmarshal(legacy, &got); err != nil {
			t.Fatalf("decode legacy result: %v", err)
		}
		if got.NoWork {
			t.Error("NoWork = true decoding a payload that predates the field; a replayed run would report " +
				"work it did as idle and the scheduler would back off a lane that is busy")
		}
		if got.Status != StatusCompleted || got.Steps != 4 || got.FinalState != "curate" {
			t.Errorf("legacy payload decoded to %+v; adding a field must not disturb the rest", got)
		}
	})

	t.Run("a result that did work encodes byte-identically to before", func(t *testing.T) {
		encoded, err := json.Marshal(RunResult{Status: StatusCompleted, FinalState: "curate", Steps: 4})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		const want = `{"status":"completed","finalState":"curate","steps":4}`
		if string(encoded) != want {
			t.Errorf("encoded = %s\nwant     = %s\nomitempty is load-bearing: without it every pre-existing "+
				"history's result changes shape", encoded, want)
		}
	})

	t.Run("a no-work result carries the field", func(t *testing.T) {
		encoded, err := json.Marshal(RunResult{Status: StatusCompleted, FinalState: "poll", Steps: 1, NoWork: true})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var round RunResult
		if err := json.Unmarshal(encoded, &round); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !round.NoWork {
			t.Errorf("NoWork did not survive the round trip: %s", encoded)
		}
	})
}

// NoWork is the RUN's accounting, not the stage's. It is set only when the
// no-work stage was the first step — a lane that claimed an item, did work, and
// then found nothing left at step four did work, and the scheduler must not
// back off because of it.
//
// The distinction is one `steps == 1` away from being wrong in a way no
// single-stage fixture would notice, so both arms are walked end to end.
func TestNoWorkOnlyWhenTheFirstStageShortCircuits(t *testing.T) {
	noWorkResult := scriptedCall{result: apiv1.ResultEnvelope{Status: apiv1.ResultNoWork}}

	t.Run("first stage", func(t *testing.T) {
		res := runNoWorkFixture(t, "nowork-first",
			fixtureSpec("poll", []apiv1.Task{detTask("poll", wf.TerminalComplete)}, nil),
			map[string][]scriptedCall{"poll": {noWorkResult}})
		if res.Steps != 1 {
			t.Fatalf("steps = %d, want 1 — the fixture must short-circuit at the first stage", res.Steps)
		}
		if !res.NoWork {
			t.Error("NoWork = false for a run whose first stage found nothing; the scheduler's idle backoff " +
				"never engages and the lane is re-polled at full rate")
		}
	})

	t.Run("later stage", func(t *testing.T) {
		res := runNoWorkFixture(t, "nowork-later",
			fixtureSpec("claim", []apiv1.Task{
				detTask("claim", "drain"),
				detTask("drain", wf.TerminalComplete),
			}, nil),
			map[string][]scriptedCall{
				"claim": {succeed(map[string]interface{}{"item": "42"})},
				"drain": {noWorkResult},
			})
		if res.Steps != 2 {
			t.Fatalf("steps = %d, want 2", res.Steps)
		}
		if res.NoWork {
			t.Error("NoWork = true for a run that completed a stage before finding nothing; that tick DID work " +
				"and backing off would stall the lane")
		}
	})
}

// runNoWorkFixture walks a fixture through the engine and returns its result.
func runNoWorkFixture(t *testing.T, runID string, spec apiv1.WorkflowSpec, script map[string][]scriptedCall) RunResult {
	t.Helper()
	_, res, err := shortcutRunWithID(t, runID, spec, script)
	if err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", res.Status, StatusCompleted)
	}
	return res
}

// The scheduler consumes NoWork through the daemon's Starter mapping, and that
// mapping keys the run's PHASE off the journal rather than off status wording —
// the correction the critic made to finding 002, and the reason PhaseForStatus
// is the exported seam it is.
//
// This test pins the join: an engine RunResult carrying NoWork produces a
// terminal a scheduler-facing mapping reads as "completed, and idle". The
// scheduler's own backoff behaviour given that pair is pinned on the far side,
// in internal/localscheduler.
func TestNoWorkTerminalIsSchedulerReadable(t *testing.T) {
	res := RunResult{Status: StatusCompleted, FinalState: "poll", Steps: 1, NoWork: true}
	got, err := PhaseForStatus(res.Status)
	if err != nil {
		t.Fatalf("PhaseForStatus(%q): %v", res.Status, err)
	}
	if got != journal.PhaseCompleted {
		t.Errorf("PhaseForStatus(%q) = %q, want %q", res.Status, got, journal.PhaseCompleted)
	}
	if !res.NoWork {
		t.Fatal("fixture is wrong")
	}

	// The correction, pinned: a hook or a scheduler must key on the PHASE, and
	// every terminal status must map to one. A status with no phase would send
	// a run into whatever the zero value happens to mean.
	for status, want := range map[string]journal.RunPhase{
		StatusCompleted: journal.PhaseCompleted,
		StatusFailed:    journal.PhaseFailed,
		StatusEscalated: journal.PhaseEscalated,
		StatusBlocked:   journal.PhaseAborted,
	} {
		got, err := PhaseForStatus(status)
		if err != nil {
			t.Errorf("PhaseForStatus(%q): %v — every terminal status must map to a phase, or a run ends up in "+
				"whatever the zero value happens to mean", status, err)
			continue
		}
		if got != want {
			t.Errorf("PhaseForStatus(%q) = %q, want %q — terminal hooks key on the journal phase, not on the "+
				"status string", status, got, want)
		}
	}
}
