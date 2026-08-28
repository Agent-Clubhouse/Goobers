package journal

import (
	"testing"
	"time"
)

// A zero lastActivity means "not observed through this handle", not "idle since
// the beginning of time". The watchdog must decline to judge rather than
// escalate, because now.Sub(zeroTime) is the maximum Duration and exceeds every
// configurable timeout — so no setting can prevent the escalation.
//
// MEASURED (#3774): a mode-3 agentic stage was escalated 11 minutes into a
// 60-minute budget, reporting "no journal progress for 2562047h47m16s (last
// activity 0001-01-01T00:00:00Z; timeout 45m0s)" while its trace showed five
// recorded events. Raising the timeout to 90m changed nothing, which is the
// tell: no finite threshold beats an unbounded baseline.
//
// lastActivity advances on Append through THIS handle and via ObserveActivity,
// which only the local runner path calls. A pod-dispatched stage writes its
// events through the journal PLANE, so the handle the watchdog holds is never
// advanced and every long mode-3 stage looks infinitely stale.
func TestIfLastActivityBeforeDeclinesToJudgeAnUnobservedRun(t *testing.T) {
	run, err := Create(t.TempDir(), testIdentity(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	// Model the handle the watchdog actually held on the cluster: one whose
	// lastActivity was never advanced. Create() appends run.started through this
	// handle and so sets it, which is exactly why the defect does not reproduce
	// for a locally-executed run — only for one whose events arrive by another
	// path entirely.
	run.mu.Lock()
	run.lastActivity = time.Time{}
	run.mu.Unlock()

	if run.IfLastActivityBefore(time.Now(), func(time.Time) {
		t.Fatal("claim ran for a run with no observed activity")
	}) {
		t.Fatal("an unobserved run was claimed as stale; that escalates healthy work and no configuration can stop it")
	}

	// Once activity IS observed, staleness is judged normally — the watchdog
	// must still do its job, which is what makes this fail-safe rather than
	// simply disabled.
	run.ObserveActivity()
	if run.IfLastActivityBefore(time.Now().Add(-time.Hour), func(time.Time) {}) {
		t.Fatal("a run active moments ago was judged stale against an hour-old cutoff")
	}
	var claimedAt time.Time
	if !run.IfLastActivityBefore(time.Now().Add(time.Hour), func(at time.Time) { claimedAt = at }) {
		t.Fatal("an observed run was not judged against a future cutoff; the watchdog must still fire")
	}
	if claimedAt.IsZero() {
		t.Fatal("claim received a zero timestamp for an observed run")
	}
}
