package localscheduler

import (
	"context"
	"testing"
	"time"
)

// An instant (tracked-only) probe must answer authoritatively even when the
// caller's context has no budget left: the regression this pins resolved
// every untracked holder fail-live under an expired parent context and
// renewed claims a mode-1 daemon must reap
// (TestUpRecoversExpiredClaimAtStartup under -race, PR #3525).
func TestProbeLiveClaimHoldersInstantPathIgnoresExpiredContext(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	probe := TrackedRunLiveness(func() []string { return []string{"tracked-run"} })
	live, err := ProbeLiveClaimHolders(ctx, []ClaimEntry{
		{RunID: "tracked-run"}, {RunID: "crashed-run"},
	}, probe)
	if err != nil {
		t.Fatalf("instant pass err = %v, want nil", err)
	}
	if !live["tracked-run"] {
		t.Fatalf("tracked-run = not live, want live")
	}
	if live["crashed-run"] {
		t.Fatalf("crashed-run = live, want DEFINITIVELY not live (authoritative in-memory answer, no fail-live)")
	}
}

// A composite carrying any non-instant member keeps the budgeted path.
func TestCompositeWithNetworkMemberIsNotInstant(t *testing.T) {
	tracked := TrackedRunLiveness(func() []string { return nil })
	composite := CompositeRunLiveness(tracked, probeFunc(func(context.Context, string) (bool, error) { return false, nil }))
	if ip, ok := composite.(instantLivenessProbe); ok && ip.InstantLiveness() {
		t.Fatalf("composite with a non-instant member reports instant; budget bypass would swallow the engine probe's bound")
	}
	trackedOnly := CompositeRunLiveness(tracked)
	ip, ok := trackedOnly.(instantLivenessProbe)
	if !ok || !ip.InstantLiveness() {
		t.Fatalf("tracked-only composite must be instant")
	}
}

type probeFunc func(ctx context.Context, runID string) (bool, error)

func (f probeFunc) RunLive(ctx context.Context, runID string) (bool, error) { return f(ctx, runID) }
