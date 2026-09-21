package readmodel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func retryBackoffFixture(at time.Time, seq uint64, stage string, branch, attempt int) journal.Event {
	e := journal.RetryBackoffEvent(stage, attempt, "local", journal.AttemptInfra, at, at.Add(time.Minute))
	e.Schema, e.Time, e.Seq, e.Branch = journal.EventSchema, at, seq, branch
	return e
}
func TestRetryBackoffLifecycleScopeAndPersistence(t *testing.T) {
	now := time.Now().UTC()
	first := retryBackoffFixture(now, 1, "work", 1, 2)
	second := retryBackoffFixture(now, 2, "other", 2, 1)
	start := journal.Event{Schema: journal.EventSchema, Seq: 3, Time: now.Add(time.Second), Type: journal.EventStageStarted, Stage: "work", Branch: 1, Attempt: 3}
	events := []journal.Event{first, second, start}
	whole := ProjectRun(testIdentity(), Projection{}, events)
	if len(whole.Run.Operator.RetryBackoff.Waits) != 1 || whole.Run.Operator.RetryBackoff.Waits[0].Stage != "other" {
		t.Fatal(whole.Run.Operator.RetryBackoff)
	}
	for split := 0; split <= len(events); split++ {
		prefix := ProjectRun(testIdentity(), Projection{}, events[:split])
		encoded, err := json.Marshal(prefix.Run.Operator)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &prefix.Run.Operator); err != nil {
			t.Fatal(err)
		}
		got := ProjectRun(testIdentity(), prefix, events[split:])
		if !reflect.DeepEqual(got.Run.Operator.RetryBackoff, whole.Run.Operator.RetryBackoff) {
			t.Fatal("incremental persistence drift", split)
		}
	}
	prior := RetryBackoffState{}.After(first)
	olderStart := start
	olderStart.Attempt = 1
	if len(prior.After(olderStart).Waits) != 1 {
		t.Fatal("old attempt erased current wait")
	}
	if len(prior.After(start).Waits) != 0 || len(prior.Waits) != 1 {
		t.Fatal("start did not clear immutably")
	}
	for _, kind := range []journal.EventType{journal.EventRunFinished, journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested, journal.EventBranchFinished} {
		e := start
		e.Type = kind
		if len(prior.After(e).Waits) != 0 {
			t.Fatal("lifecycle did not clear", kind)
		}
	}
	reset := journal.Event{Schema: journal.EventSchema, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": journal.RetryBackoffResetKind}}
	if len(prior.After(reset).Waits) != 0 {
		t.Fatal("crash resume retained local timer")
	}
}
func TestRetryBackoffRejectsUnknownEvidenceAndBounds(t *testing.T) {
	now := time.Now().UTC()
	for _, mutate := range []func(*journal.Event){
		func(e *journal.Event) { e.Runner["kind"] = "retry-backoff.v2" },
		func(e *journal.Event) { e.Runner["driver"] = "unknown" },
		func(e *journal.Event) { e.Runner["retryClass"] = "human" },
		func(e *journal.Event) { e.Runner["deadline"] = now.Add(-time.Second).Format(time.RFC3339Nano) },
		func(e *journal.Event) { e.Runner["observedAt"] = now.Add(time.Second).Format(time.RFC3339Nano) },
		func(e *journal.Event) { e.Attempt = 0 },
	} {
		e := retryBackoffFixture(now, 1, "work", 0, 1)
		mutate(&e)
		if got := (RetryBackoffState{}).After(e); len(got.Waits) != 0 {
			t.Fatal(got)
		}
	}
	var state RetryBackoffState
	for i := 0; i <= MaxRetryBackoffs; i++ {
		state = state.After(retryBackoffFixture(now, uint64(i+1), fmt.Sprint(i), i, 1))
	}
	if !state.Truncated || len(state.Waits) != MaxRetryBackoffs {
		t.Fatal(state)
	}
}
