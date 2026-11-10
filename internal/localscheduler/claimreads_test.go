package localscheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/journal"
)

func TestFileClaimReadAdaptersPreserveBytesAndNilShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // In-memory local snapshots never depended on a caller deadline.
	check := func() {
		t.Helper()
		for name, pair := range map[string]struct {
			old  []ClaimEntry
			read func() ([]ClaimEntry, error)
		}{
			"active":  {ledger.Snapshot(), func() ([]ClaimEntry, error) { return ledger.SnapshotErr(ctx) }},
			"history": {ledger.HistorySnapshot(), func() ([]ClaimEntry, error) { return ledger.HistorySnapshotErr(ctx) }},
			"run":     {ledger.ForRunAll("run-a"), func() ([]ClaimEntry, error) { return ledger.ForRunAllErr(ctx, "run-a") }},
			"absent":  {ledger.ForRunAll("absent"), func() ([]ClaimEntry, error) { return ledger.ForRunAllErr(ctx, "absent") }},
		} {
			got, err := pair.read()
			before, _ := json.Marshal(pair.old)
			after, _ := json.Marshal(got)
			if err != nil || !reflect.DeepEqual(got, pair.old) || !bytes.Equal(before, after) {
				t.Fatalf("%s changed: %s -> %s (%v)", name, before, after, err)
			}
		}
	}
	check()
	for _, id := range []string{"b", "a"} {
		if ok, _, err := ledger.Claim(id, "run-a", "wf", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if err := ledger.Release("b", "run-a", claimrelease.Completed.Reason()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read changed persisted local ledger: %v", err)
	}
}

func TestClaimReadFailureDefersScheduleAndRecoversWithoutLosingDemand(t *testing.T) {
	for name, cause := range map[string]error{"deadline": context.DeadlineExceeded, "query": errors.New("database unavailable")} {
		t.Run(name, func(t *testing.T) {
			starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
			counter := &fakeBacklogCounter{err: errors.Join(ErrClaimRead, cause)}
			sched, dir := newTestScheduler(t, []WorkflowEntry{{Workflow: "claim-aware", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 2}, Schedules: []Schedule{fakeSchedule{d: time.Hour}}, ScheduleDemandCounter: counter, Starter: starter}})
			tickDueSchedule(t, sched, "claim-aware", time.Hour)
			sched.Wait()
			if starter.count() != 0 {
				t.Fatal("claim-read failure synthesized a start")
			}
			id := WorkflowIdentity{Workflow: "claim-aware"}
			pending, err := readScheduleDemandState(dir)
			if err != nil || !pending[id] || !sched.pendingScheduleDemand[id].repoll {
				t.Fatalf("lost pending demand: %v %v", pending, err)
			}
			if n := countErrorCode(t, dir, "schedule_demand_count_failed"); n != 1 {
				t.Fatalf("reported read failures=%d, want 1", n)
			}
			counter.err = nil
			counter.setCount(1)
			tickDueSchedule(t, sched, "claim-aware", time.Second)
			sched.Wait()
			if starter.count() != 1 {
				t.Fatalf("starts after recovery=%d, want 1", starter.count())
			}
			pending, err = readScheduleDemandState(dir)
			if err != nil || pending[id] {
				t.Fatalf("settled demand remained: %v %v", pending, err)
			}
		})
	}
}

// Existing local counters do not return ErrClaimRead: retain #5605's timeout
// fallback, ordinary zero-demand clearing, and backlog/refill refusal verbatim.
func TestClaimReadMarkerDoesNotChangeOrdinaryDemandFailurePolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		schedule bool
		want     int
	}{
		{"provider timeout", context.DeadlineExceeded, true, 1},
		{"provider transient", errors.New("GET /pulls failed: status 502"), true, 1},
		{"ordinary error", errors.New("bad request"), true, 0},
		{"backlog timeout", context.DeadlineExceeded, false, 0},
		{"claim timeout", errors.Join(ErrClaimRead, context.DeadlineExceeded), true, 0},
	} {
		if got := failedPollDemand(t.Context(), demandPoll{schedule: test.schedule}, test.err); got != test.want {
			t.Errorf("%s demand=%d, want %d", test.name, got, test.want)
		}
	}
}
