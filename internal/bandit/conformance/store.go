// Package conformance checks the shared local/cloud observation-store contract.
package conformance

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/bandit"
)

// Run checks idempotency, attempt isolation, concurrent conflicts and ordering.
// Each subtest receives a fresh store; implementations may reopen durable state.
func Run(t *testing.T, open func(*testing.T) bandit.ObservationStore) {
	t.Helper()
	t.Run("retry-and-conflict", func(t *testing.T) {
		s := open(t)
		r := Record("run-a", "build", 0, 1, 1)
		for range 2 {
			if err := s.Record(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
		changed := r
		changed.Observation.Arm = "other"
		if err := s.Record(t.Context(), changed); !errors.Is(err, bandit.ErrObservationConflict) {
			t.Fatalf("conflicting retry = %v", err)
		}
		got, err := s.Observations(t.Context(), "build")
		if err != nil || !reflect.DeepEqual(got, []bandit.Observation{r.Observation}) {
			t.Fatalf("committed result changed: %v, %v", got, err)
		}
	})
	t.Run("attempt-identities-and-stage-scope", func(t *testing.T) {
		s := open(t)
		records := []bandit.ObservationRecord{
			Record("run-a", "build", 0, 1, 1), Record("run-a", "build", 0, 1, 2),
			Record("run-a", "build", 0, 2, 1), Record("run-a", "build", 1, 1, 1),
			Record("run-b", "build", 0, 1, 1), Record("run-a", "test", 0, 1, 1),
		}
		for i, r := range records {
			r.Observation.Assigned = uint64(len(records) - i)
			if err := s.Record(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Observations(t.Context(), "build")
		if err != nil || len(got) != 5 {
			t.Fatalf("sample domain = %v, %v", got, err)
		}
		for i, o := range got {
			if o.Stage != "build" || o.Assigned != uint64(i+2) {
				t.Fatalf("sample ordering/scope = %v", got)
			}
		}
	})
	t.Run("concurrent-publication", func(t *testing.T) {
		s := open(t)
		a := Record("run-a", "build", 0, 1, 1)
		b := a
		b.Observation.Arm = "other"
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Go(func() {
				r := a
				if i%2 == 1 {
					r = b
				}
				errs <- s.Record(t.Context(), r)
			})
		}
		wg.Wait()
		close(errs)
		var wins, conflicts int
		for err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, bandit.ErrObservationConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if wins != 8 || conflicts != 8 {
			t.Fatalf("wins=%d conflicts=%d; one immutable result must win", wins, conflicts)
		}
		got, err := s.Observations(t.Context(), "build")
		if err != nil || len(got) != 1 {
			t.Fatalf("concurrent results = %v, %v", got, err)
		}
	})
	t.Run("invalid-and-cancelled", func(t *testing.T) {
		s := open(t)
		r := Record("run-a", "build", 0, 1, 1)
		invalid := r
		invalid.Observation.RunID = "run-b"
		if err := s.Record(t.Context(), invalid); err == nil {
			t.Fatal("accepted mismatching run identity")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.Record(ctx, r); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled write = %v", err)
		}
		if _, err := s.Observations(ctx, "build"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled read = %v", err)
		}
		if got, err := s.Observations(t.Context(), "build"); err != nil || len(got) != 0 {
			t.Fatalf("refused write left samples: %v, %v", got, err)
		}
	})
}

// Record supplies a valid outcome with an explicitly identified task attempt.
func Record(runID, stage string, branch int, occurrence int64, attempt int) bandit.ObservationRecord {
	return bandit.ObservationRecord{
		Key:         bandit.ObservationKey{RunID: runID, Stage: stage, Branch: branch, Occurrence: occurrence, Attempt: attempt},
		Observation: bandit.Observation{Schema: bandit.Schema, RunID: runID, Stage: stage, Arm: "control", Window: "train", Reward: 1, RewardSet: true, Success: true, Assigned: 1},
	}
}
