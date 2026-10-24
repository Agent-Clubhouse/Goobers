//go:build cloudintegration

package blockedrecordspg_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// escalations records what the sweep asked to escalate.
type escalations struct {
	mu     sync.Mutex
	cycles []blockedcycle.Result
	err    error
}

func (e *escalations) escalate(_ context.Context, cycle blockedcycle.Result, _ blockedrecordspg.Deferral) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.cycles = append(e.cycles, cycle)
	return nil
}

func (e *escalations) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cycles)
}

type deferralRig struct {
	admin   *pgxpool.Pool
	manager *lease.Manager
	handle  *lease.Handle
	closed  *closedSet
	budget  bool
	store   blockedrecordspg.Store
	esc     *escalations
}

func newDeferralRig(t *testing.T) *deferralRig {
	t.Helper()
	admin := pgtest.Pool(t, "brdefer")
	manager := pgtest.ControllerManager(t, admin, "leader", 0)
	r := &deferralRig{admin: admin, manager: manager, handle: pgtest.Acquire(t, manager), closed: &closedSet{closed: map[string]bool{}}, budget: true, esc: &escalations{}}
	r.store = blockedrecordspg.Store{
		Fence: manager, Closed: r.closed.check, Defer: true, RetryDelays: []time.Duration{},
		Reserve: func(providers.ProviderKind) bool { return r.budget },
	}
	return r
}

// deferCycle records a 1<->2 cycle while the budget is spent, so it is deferred.
func (r *deferralRig) deferCycle(t *testing.T) {
	t.Helper()
	if _, err := r.store.RecordBlock(t.Context(), r.handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	r.budget = false
	cycle, err := r.store.RecordBlock(t.Context(), r.handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("deferring record: cycle %+v, err %v; want the cycle deferred (nothing returned)", cycle, err)
	}
	r.budget = true
	if n, err := r.store.Deferrals(t.Context(), r.handle); err != nil || n != 1 {
		t.Fatalf("deferrals = %d, %v; want one durable marker", n, err)
	}
}

func (r *deferralRig) makeDue(t *testing.T, deferredFor time.Duration) {
	t.Helper()
	if _, err := r.admin.Exec(t.Context(), `UPDATE control.blocked_record
		SET verify_after = now() - interval '1 second', deferred_since = now() - $1::interval WHERE verify_after IS NOT NULL`, deferredFor); err != nil {
		t.Fatal(err)
	}
}

func (r *deferralRig) sweep(t *testing.T, store blockedrecordspg.Store, handle *lease.Handle) (blockedrecordspg.SweepResult, error) {
	t.Helper()
	return store.Sweep(t.Context(), handle, blockedrecordspg.SweepOptions{Bound: time.Hour, Escalate: r.esc.escalate})
}

func TestADeferredCycleIsNotDueUntilItsMarkerSaysSoAndIsThenEscalatedOnceWhenReal(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	if res, err := r.sweep(t, r.store, r.handle); err != nil || !nothingDone(res) || r.esc.count() != 0 {
		t.Fatalf("sweep before the marker is due: %+v, %v, escalations %d", res, err, r.esc.count())
	}
	r.makeDue(t, 10*time.Minute)
	res, err := r.sweep(t, r.store, r.handle)
	if err != nil || res.Escalated != 1 || r.esc.count() != 1 {
		t.Fatalf("sweep: %+v, %v, escalations %d; want the verified real cycle escalated once", res, err, r.esc.count())
	}
	if got := r.esc.cycles[0]; len(got.Affected) != 2 || len(got.Unverified) != 0 {
		t.Fatalf("escalated %+v; want both members, none unverified (every member was read open)", got)
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals after escalation = %d, want the marker cleared", n)
	}
	r.makeDue(t, 0) // nothing marked: nothing happens
	if res, err := r.sweep(t, r.store, r.handle); err != nil || !nothingDone(res) || r.esc.count() != 1 {
		t.Fatalf("a second sweep: %+v, %v, escalations %d; want exactly one escalation in total", res, err, r.esc.count())
	}
}

func TestADeferredCycleWithAClosedMemberIsDroppedSilently(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	r.closed.closed["1"] = true // item 1 (not the seed, 2) closed in the meantime
	r.makeDue(t, 10*time.Minute)
	res, err := r.sweep(t, r.store, r.handle)
	if err != nil || res.Dropped != 1 || r.esc.count() != 0 {
		t.Fatalf("sweep: %+v, %v, escalations %d; want the false cycle dropped without an escalation", res, err, r.esc.count())
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d, want none", n)
	}
}

// The seed is itself a member, so closing it dissolves the cycle even though the
// others remain; its record (and the marker on it) go with it.
func TestForgettingTheSeedClearsTheMarkerAndEscalatesNothing(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t) // the seed is item 2
	r.closed.closed["2"] = true
	r.makeDue(t, 10*time.Minute)
	if res, err := r.sweep(t, r.store, r.handle); err != nil || r.esc.count() != 0 {
		t.Fatalf("sweep: %+v, %v, escalations %d; want nothing escalated", res, err, r.esc.count())
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d after the seed closed", n)
	}
}

func TestAStillUnverifiableCycleIsDeferredAgainThenEscalatedAtTheBoundWithNotes(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	r.budget = false
	r.makeDue(t, 10*time.Minute)
	res, err := r.sweep(t, r.store, r.handle)
	if err != nil || res.Deferred != 1 || r.esc.count() != 0 {
		t.Fatalf("sweep under a spent budget: %+v, %v, escalations %d; want it deferred again", res, err, r.esc.count())
	}
	var attempts int
	var verifyAfterFuture bool
	if err := r.admin.QueryRow(t.Context(), `SELECT defer_attempts, verify_after > now() FROM control.blocked_record WHERE verify_after IS NOT NULL`).Scan(&attempts, &verifyAfterFuture); err != nil || attempts != 2 || !verifyAfterFuture {
		t.Fatalf("marker attempts %d future %v (%v); want a second attempt pushed into the future", attempts, verifyAfterFuture, err)
	}
	// Past the bound it ends, escalated with the members named.
	r.makeDue(t, 2*time.Hour)
	res, err = r.sweep(t, r.store, r.handle)
	if err != nil || res.Escalated != 1 || r.esc.count() != 1 {
		t.Fatalf("sweep past the bound: %+v, %v, escalations %d; want the cycle escalated", res, err, r.esc.count())
	}
	if got := r.esc.cycles[0]; len(got.Unverified) != 2 || !strings.Contains(got.Unverified[0], "unverifiable: rate limited") {
		t.Fatalf("notes = %q; want both members named as unverifiable", got.Unverified)
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d, want none after the bound", n)
	}
}

func TestAPermanentFailureEndsTheDeferralAtOnce(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	r.closed.err = &blockedrecordspg.VerifyError{Class: "not found", Permanent: true, Message: "gone"}
	r.makeDue(t, 10*time.Minute) // well inside the bound
	if res, err := r.sweep(t, r.store, r.handle); err != nil || res.Escalated != 1 || !strings.Contains(strings.Join(r.esc.cycles[0].Unverified, ";"), "unverifiable: not found") {
		t.Fatalf("sweep: %+v, %v; want an immediate escalation naming the reason", res, err)
	}
}

// A failed escalation keeps the marker, so the next pass retries it; the cycle is
// identified by its members, so the retried writes carry the same key.
func TestAFailedEscalationKeepsTheMarkerAndIsRetried(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	r.makeDue(t, 10*time.Minute)
	r.esc.err = errors.New("provider unavailable")
	if _, err := r.sweep(t, r.store, r.handle); err == nil {
		t.Fatal("a failed escalation was not reported")
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 1 {
		t.Fatalf("deferrals = %d after a failed escalation, want the marker kept", n)
	}
	r.esc.err = nil
	res, err := r.sweep(t, r.store, r.handle)
	if err != nil || res.Escalated != 1 || r.esc.count() != 1 {
		t.Fatalf("retry: %+v, %v, escalations %d", res, err, r.esc.count())
	}
}

// The marker is durable: a successor leader finds the predecessor's deferred cycle
// and resolves it, once; the deposed leader's sweep is refused.
func TestADeferredCycleSurvivesAFailover(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	r.makeDue(t, 10*time.Minute)

	successorManager := pgtest.ControllerManager(t, r.admin, "successor", 0)
	successor := pgtest.Takeover(t, r.admin, successorManager)
	if _, err := r.sweep(t, r.store, r.handle); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("the deposed leader's sweep = %v, want ErrStale", err)
	}
	if r.esc.count() != 0 {
		t.Fatal("a deposed leader escalated")
	}
	next := r.store
	next.Fence = successorManager
	res, err := r.sweep(t, next, successor)
	if err != nil || res.Escalated != 1 || r.esc.count() != 1 {
		t.Fatalf("successor sweep: %+v, %v, escalations %d; want the predecessor's deferred cycle escalated once", res, err, r.esc.count())
	}
	if n, _ := next.Deferrals(t.Context(), successor); n != 0 {
		t.Fatalf("deferrals = %d, want none", n)
	}
}

// Without a driver nothing may be deferred: an unverifiable cycle is escalated at
// once, labelled, so it is never parked with nobody to escalate it.
func TestWithoutADriverAnUnverifiableCycleIsEscalatedNotDeferred(t *testing.T) {
	r := newDeferralRig(t)
	r.store.Defer = false
	if _, err := r.store.RecordBlock(t.Context(), r.handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	r.budget = false
	cycle, err := r.store.RecordBlock(t.Context(), r.handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 2 || len(cycle.Unverified) == 0 {
		t.Fatalf("cycle %+v, err %v; want an escalated, labelled cycle", cycle, err)
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d, want none without a driver", n)
	}
}

// A new block on the seed supersedes its deferral.
func TestARecordedBlockSupersedesItsOwnDeferral(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	if _, err := r.store.RecordBlock(t.Context(), r.handle, block("2", "9")); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d after the seed was re-recorded without a cycle", n)
	}
}

// Azure DevOps is never deferred: the instance-level writer cannot comment there,
// and the labels would go with the refused comment, so a deferred ADO cycle could
// never be escalated.
func TestACycleOnAzureDevOpsIsEscalatedNowNotDeferred(t *testing.T) {
	r := newDeferralRig(t)
	ado := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "board", Name: "board"}
	blockADO := func(item, on string) parking.Block {
		b := block(item, on)
		b.Repository = ado
		return b
	}
	if _, err := r.store.RecordBlock(t.Context(), r.handle, blockADO("1", "2")); err != nil {
		t.Fatal(err)
	}
	r.budget = false
	cycle, err := r.store.RecordBlock(t.Context(), r.handle, blockADO("2", "1"))
	if err != nil || len(cycle.Affected) != 2 || len(cycle.Unverified) != 2 {
		t.Fatalf("cycle %+v, err %v; want an immediate, labelled escalation", cycle, err)
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d; an ADO cycle must never be deferred", n)
	}
}

func permanentRefusal(msg string) error {
	return fmt.Errorf("%s: %w", msg, providerwriter.ErrPermanent)
}

// refuse drives a deferred cycle to a permanent escalation refusal.
func (r *deferralRig) refuse(t *testing.T) {
	t.Helper()
	r.deferCycle(t)
	r.makeDue(t, 10*time.Minute)
	r.esc.err = errors.Join(permanentRefusal("comment on #1"), permanentRefusal("comment on #2"))
	res, err := r.sweep(t, r.store, r.handle)
	if !errors.Is(err, blockedrecordspg.ErrEscalationRefused) || len(res.NewlyRefused) != 1 || len(res.Refused) != 1 {
		t.Fatalf("sweep: %+v, %v; want the refusal reported with the cycle named", res, err)
	}
	r.esc.err = nil
}

func (r *deferralRig) refused(t *testing.T, store blockedrecordspg.Store, handle *lease.Handle) []string {
	t.Helper()
	keys, err := store.Refused(t.Context(), handle)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// A permanent refusal of the escalation itself ends the retries and is latched in
// the row: nothing is retried in silence, and the refusal stays visible.
func TestAPermanentEscalationRefusalStopsTheRetriesAndStaysLatched(t *testing.T) {
	r := newDeferralRig(t)
	r.refuse(t)
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 0 {
		t.Fatalf("deferrals = %d; a refused cycle must not stay due", n)
	}
	if got := r.refused(t, r.store, r.handle); len(got) != 1 {
		t.Fatalf("refused = %v; want the cycle latched", got)
	}
	// Nothing further is attempted, and the latch persists across passes.
	r.makeDue(t, 10*time.Minute)
	res, err := r.sweep(t, r.store, r.handle)
	if err != nil || !nothingDone(res) || len(res.Refused) != 1 || r.esc.count() != 0 {
		t.Fatalf("next sweep: %+v, %v; want nothing retried and the refusal still standing", res, err)
	}
}

// The latch is in the database, so a successor leader sees it; the deposed one is
// fenced out.
func TestARefusedCycleSurvivesAFailover(t *testing.T) {
	r := newDeferralRig(t)
	r.refuse(t)
	successorManager := pgtest.ControllerManager(t, r.admin, "successor", 0)
	successor := pgtest.Takeover(t, r.admin, successorManager)
	next := r.store
	next.Fence = successorManager
	if got := r.refused(t, next, successor); len(got) != 1 {
		t.Fatalf("successor sees refused = %v, want the predecessor's refusal", got)
	}
	res, err := r.sweep(t, next, successor)
	if err != nil || len(res.Refused) != 1 || len(res.NewlyRefused) != 0 {
		t.Fatalf("successor sweep: %+v, %v; want the refusal standing and not re-announced", res, err)
	}
}

// A refusal clears itself with the condition: a re-recorded block on the seed, the
// seed being forgotten, or the cycle no longer being found.
func TestARefusedCycleClearsWhenTheCycleResolves(t *testing.T) {
	t.Run("seed re-recorded", func(t *testing.T) {
		r := newDeferralRig(t)
		r.refuse(t)
		if _, err := r.store.RecordBlock(t.Context(), r.handle, block("2", "9")); err != nil {
			t.Fatal(err)
		}
		if got := r.refused(t, r.store, r.handle); len(got) != 0 {
			t.Fatalf("refused = %v after the seed was re-recorded", got)
		}
	})
	t.Run("seed forgotten", func(t *testing.T) {
		r := newDeferralRig(t)
		r.refuse(t)
		r.closed.closed["2"] = true
		if err := r.store.Forget(t.Context(), r.handle, []blockedcycle.Node{{Repository: block("2").Repository, ItemID: "2"}}); err != nil {
			t.Fatal(err)
		}
		if got := r.refused(t, r.store, r.handle); len(got) != 0 {
			t.Fatalf("refused = %v after the seed was forgotten", got)
		}
	})
	t.Run("cycle gone but seed kept", func(t *testing.T) {
		r := newDeferralRig(t)
		r.refuse(t)
		// The other member's record disappears (its item closed): the seed row
		// remains but no cycle runs through it.
		if _, err := r.admin.Exec(t.Context(), `DELETE FROM control.blocked_record WHERE record_key <> (SELECT record_key FROM control.blocked_record WHERE starts_with(defer_reason, 'refused: '))`); err != nil {
			t.Fatal(err)
		}
		res, err := r.sweep(t, r.store, r.handle)
		if err != nil || len(res.Refused) != 0 {
			t.Fatalf("sweep: %+v, %v; want the refusal released", res, err)
		}
		if got := r.refused(t, r.store, r.handle); len(got) != 0 {
			t.Fatalf("refused = %v after the cycle was gone", got)
		}
	})
}

// One member's permanent refusal must not stop a cycle whose other members only
// failed transiently: that cycle is retried, and refused only once every remaining
// failure is permanent.
func TestOneMembersPermanentRefusalDoesNotStopATransientlyFailingCycle(t *testing.T) {
	r := newDeferralRig(t)
	r.deferCycle(t)
	r.makeDue(t, 10*time.Minute)
	r.esc.err = errors.Join(permanentRefusal("comment on #1"), errors.New("provider unavailable on #2"))
	res, err := r.sweep(t, r.store, r.handle)
	if err == nil || errors.Is(err, blockedrecordspg.ErrEscalationRefused) || len(res.Refused) != 0 {
		t.Fatalf("sweep: %+v, %v; want a retryable failure, not a refusal", res, err)
	}
	if n, _ := r.store.Deferrals(t.Context(), r.handle); n != 1 {
		t.Fatalf("deferrals = %d, want the marker kept so the transient member is retried", n)
	}
	// Once the transient member recovers, only the permanent refusal remains.
	r.esc.err = permanentRefusal("comment on #1")
	if res, err := r.sweep(t, r.store, r.handle); !errors.Is(err, blockedrecordspg.ErrEscalationRefused) || len(res.Refused) != 1 {
		t.Fatalf("sweep: %+v, %v; want the cycle refused once every failure is permanent", res, err)
	}
}

func nothingDone(res blockedrecordspg.SweepResult) bool {
	return res.Dropped == 0 && res.Escalated == 0 && res.Deferred == 0 && len(res.NewlyRefused) == 0
}
