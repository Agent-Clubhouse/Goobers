//go:build cloudintegration

package blockedrecordspg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/providers"
)

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

func block(item string, blockers ...string) parking.Block {
	return parking.Block{Repository: repo, ItemID: item, Blockers: blockers, RunID: "run-" + item, Stage: "implement", Reason: "waits"}
}

func TestACycleIsDetectedAcrossLeaseEpochs(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	oldManager := pgtest.ControllerManager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	oldStore := blockedrecordspg.Store{Fence: oldManager}
	if cycle, err := oldStore.RecordBlock(t.Context(), old, block("1", "2")); err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("first block: %+v, %v; want no cycle", cycle, err)
	}
	// The block that closes the cycle is recorded by the successor leader: the
	// predecessor's record must have survived the lease change.
	newManager := pgtest.ControllerManager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	newStore := blockedrecordspg.Store{Fence: newManager}
	cycle, err := newStore.RecordBlock(t.Context(), successor, block("2", "pr/1"))
	if err != nil || len(cycle.Affected) != 2 {
		t.Fatalf("closing block: %+v, %v; want a 2-member cycle", cycle, err)
	}
	// A stale leader can neither record nor read the graph.
	if _, err := oldStore.RecordBlock(t.Context(), old, block("3", "4")); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader RecordBlock = %v, want ErrStale", err)
	}
	// Re-recording an item replaces its record.
	if cycle, err := newStore.RecordBlock(t.Context(), successor, block("2", "9")); err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("replaced block still reports a cycle: %+v, %v", cycle, err)
	}
}

type closedSet struct {
	mu     sync.Mutex
	closed map[string]bool
	err    error
	reads  int
}

func (c *closedSet) check(_ context.Context, _ string, _ providers.RepositoryRef, item string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.closed[item], c.err
}

// A record naming a blocker that has since closed must not close a cycle: that
// would escalate a non-cyclic item needs-human.
func TestAClosedBlockerCannotFormAFalseCycle(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	closed := &closedSet{closed: map[string]bool{}}
	store := blockedrecordspg.Store{Fence: manager, Closed: closed.check, RetryDelays: []time.Duration{time.Millisecond}}
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	// Item 2 closed since 1 recorded its block on it; 2 is then (re)blocked on 1.
	closed.closed["2"] = true
	cycle, err := store.RecordBlock(t.Context(), handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("closed blocker formed a cycle: %+v, %v", cycle, err)
	}
	// An unverifiable item keeps the cycle: dropping on a guess would hide a real one.
	closed.closed["2"], closed.err = false, errors.New("provider unavailable")
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	cycle, err = store.RecordBlock(t.Context(), handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 2 {
		t.Fatalf("unverifiable members dropped a cycle: %+v, %v", cycle, err)
	}
}

func TestHealForgetsClosedItemsAndTheirTrailAndIsFenced(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	controller := pgtest.ControllerPool(t, pool)
	oldManager := pgtest.ControllerManager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	closed := &closedSet{closed: map[string]bool{"2": true}}
	store := blockedrecordspg.Store{Fence: oldManager, Closed: closed.check}
	for _, b := range []parking.Block{block("1", "2", "3"), block("2", "9"), block("4", "2")} {
		if _, err := store.RecordBlock(t.Context(), old, b); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		if err := controller.QueryRow(t.Context(), `SELECT count(*) FROM control.blocked_record`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 3 {
		t.Fatalf("records = %d, want 3", count())
	}
	newManager := pgtest.ControllerManager(t, pool, "new", 0)
	successor := pgtest.Takeover(t, pool, newManager)
	if _, err := store.Heal(t.Context(), old, 100); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale leader Heal = %v, want ErrStale", err)
	}
	next := blockedrecordspg.Store{Fence: newManager, Closed: closed.check}
	forgot, err := next.Heal(t.Context(), successor, 100)
	if err != nil || forgot != 1 {
		t.Fatalf("Heal = %d, %v; want exactly item 2 forgotten", forgot, err)
	}
	// 2's own record is gone, 4 (blocked only on 2) is gone, 1 keeps blocker 3.
	if count() != 1 {
		t.Fatalf("records after heal = %d, want 1", count())
	}
	var raw string
	if err := controller.QueryRow(t.Context(), `SELECT record::text FROM control.blocked_record`).Scan(&raw); err != nil || !strings.Contains(raw, `"3"`) || strings.Contains(raw, `"2"`) {
		t.Fatalf("remaining record = %s, %v", raw, err)
	}
}

// When the read budget is spent before the cycle's members are verified, the
// cycle is NOT parked silently: nothing would ever re-run the decision, so it is
// escalated, with the unverified members named.
func TestACycleUnderASpentBudgetIsEscalatedLabelledUnverified(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	closed := &closedSet{closed: map[string]bool{}}
	store := blockedrecordspg.Store{Fence: manager, Closed: closed.check, Reserve: func(providers.ProviderKind) bool { return false }}
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	cycle, err := store.RecordBlock(t.Context(), handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 2 {
		t.Fatalf("cycle %+v, err %v; want the two-member cycle escalated", cycle, err)
	}
	if len(cycle.Unverified) != 2 || !strings.Contains(cycle.Unverified[0], "unverifiable: rate limited") {
		t.Fatalf("notes = %q; want both members labelled unverifiable", cycle.Unverified)
	}
	if closed.reads != 0 {
		t.Fatalf("the provider was read %d times with no budget", closed.reads)
	}
}

// A retryable failure is retried inline, then the cycle is escalated with the
// member named; a recovery on a retry gives a plain verified answer.
func TestARetryableFailureIsRetriedInlineThenEscalated(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	closed := &closedSet{closed: map[string]bool{}, err: errors.New("GET /issues/2 failed: status 503: unavailable")}
	delays := []time.Duration{time.Millisecond, time.Millisecond}
	store := blockedrecordspg.Store{Fence: manager, Closed: closed.check, RetryDelays: delays}
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	closed.reads = 0
	cycle, err := store.RecordBlock(t.Context(), handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 2 {
		t.Fatalf("cycle %+v, err %v; want the cycle escalated after the retries", cycle, err)
	}
	// Both members fail: each is read once plus once per retry delay.
	if want := 2 * (1 + len(delays)); closed.reads != want {
		t.Fatalf("reads = %d, want %d (every member, then the retries)", closed.reads, want)
	}
	if len(cycle.Unverified) != 2 || !strings.Contains(cycle.Unverified[0], "unverifiable: provider unavailable") {
		t.Fatalf("notes = %q", cycle.Unverified)
	}
	// The provider recovers: the same block now verifies cleanly.
	closed.err = nil
	cycle, err = store.RecordBlock(t.Context(), handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 2 || len(cycle.Unverified) != 0 {
		t.Fatalf("after recovery: cycle %+v, err %v; want a verified cycle with no notes", cycle, err)
	}
}

func TestAPermanentVerificationFailureEscalatesAtOnceWithoutRetries(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	closed := &closedSet{closed: map[string]bool{}, err: &blockedrecordspg.VerifyError{Class: "not found", Permanent: true, Message: "[REDACTED]"}}
	store := blockedrecordspg.Store{Fence: manager, Closed: closed.check, RetryDelays: []time.Duration{time.Millisecond, time.Millisecond}}
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	closed.reads = 0
	cycle, err := store.RecordBlock(t.Context(), handle, block("2", "1"))
	if err != nil || len(cycle.Affected) != 2 || len(cycle.Unverified) == 0 || !strings.Contains(cycle.Unverified[0], "unverifiable: not found") {
		t.Fatalf("permanent failure: cycle %+v, err %v; want an immediate escalation naming the reason", cycle, err)
	}
	if closed.reads != 2 {
		t.Fatalf("reads = %d, want one per member and no retries", closed.reads)
	}
}

// A block being decided now verifies its cycle even for an item the heal has
// backed off, and the outcome clears the backoff.
func TestRecordBlockIgnoresTheHealBackoff(t *testing.T) {
	pool := pgtest.Pool(t, "brpg")
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	closed := &closedSet{closed: map[string]bool{}}
	backoff := &blockedrecordspg.Backoff{}
	store := blockedrecordspg.Store{Fence: manager, Closed: closed.check, Backoff: backoff}
	if _, err := store.RecordBlock(t.Context(), handle, block("1", "2")); err != nil {
		t.Fatal(err)
	}
	node := blockedcycle.Node{Repository: repo, ItemID: "2"}
	backoff.Failed(node) // the heal could not verify 2 a moment ago
	closed.closed["2"] = true
	if cycle, err := store.RecordBlock(t.Context(), handle, block("2", "1")); err != nil || len(cycle.Affected) != 0 {
		t.Fatalf("cycle %+v, err %v; a backed-off but closed blocker must still be verified when a block is decided", cycle, err)
	}
	if !backoff.Allowed(node) {
		t.Fatal("the verification did not clear the item's backoff")
	}
}
