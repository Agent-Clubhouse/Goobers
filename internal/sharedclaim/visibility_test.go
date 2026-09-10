package sharedclaim

import (
	"context"
	"errors"
	"testing"
	"time"
)

type visibilityFixture struct {
	present bool
	writes  int
	err     error
	readErr error
	reads   int
	onRead  func()
	onWrite func(bool)
}

func (v *visibilityFixture) ReadClaimed(context.Context, string) (bool, error) {
	v.reads++
	if v.onRead != nil {
		v.onRead()
	}
	return v.present, v.readErr
}

func TestVisibilityRejectsMalformedAuthorityBeforeLabelIO(t *testing.T) {
	for _, observed := range []Observation{
		{},
		{Now: time.Now(), Record: Record{Version: 1}},
		{Now: time.Now(), Revision: "1", Record: Record{Version: 2}},
		{Now: time.Now(), Revision: "1", Record: Record{Version: 1, Owner: Owner{Instance: "partial"}}},
	} {
		labels := &visibilityFixture{}
		if err := ReconcileVisibility(t.Context(), &memoryStore{observation: observed}, labels, "42"); err == nil {
			t.Fatal("invalid authority accepted")
		}
		if labels.reads != 0 || labels.writes != 0 {
			t.Fatal("invalid authority reached label IO")
		}
	}
}

func TestVisibilityBoundsChurnAndPropagatesReadFailure(t *testing.T) {
	s := &memoryStore{observation: Observation{Now: time.Now(), Revision: "1", Record: Record{Version: 1}}}
	labels := &visibilityFixture{readErr: errors.New("read failed")}
	if err := ReconcileVisibility(t.Context(), s, labels, "42"); !errors.Is(err, labels.readErr) || labels.writes != 0 {
		t.Fatalf("read failure was not preserved: %v", err)
	}
	labels.readErr, labels.reads = nil, 0
	labels.onRead = func() { s.observation.Revision += "x" }
	if err := ReconcileVisibility(t.Context(), s, labels, "42"); !errors.Is(err, ErrConflict) || labels.reads != 3 || s.writes != 0 {
		t.Fatalf("unbounded or authoritative churn: %v", err)
	}
}

func TestVisibilityDetectsExpiryWithoutRevisionChange(t *testing.T) {
	now := time.Now()
	s := &memoryStore{observation: Observation{Now: now, Revision: "1", Record: Record{Version: 1, Owner: Owner{"instance", "run", "token"}, ExpiresAt: now.Add(time.Second)}}}
	labels := &visibilityFixture{}
	labels.onWrite = func(bool) { s.observation.Now = now.Add(time.Second) }
	if err := ReconcileVisibility(t.Context(), s, labels, "42"); err != nil || labels.present || labels.writes != 2 {
		t.Fatalf("expiry during label write was missed: %v", err)
	}
}

func (v *visibilityFixture) SetClaimed(_ context.Context, _ string, value bool) error {
	v.writes++
	if v.err != nil {
		return v.err
	}
	v.present = value
	if v.onWrite != nil {
		v.onWrite(value)
	}
	return nil
}

func TestVisibilityTracksOwnershipWithoutChangingAuthority(t *testing.T) {
	now := time.Now().UTC()
	owner := Owner{"instance", "run", "token"}
	for _, tc := range []struct {
		name         string
		rev          string
		owner        Owner
		expires      time.Time
		before, want bool
	}{
		{"live", "1", owner, now.Add(time.Minute), false, true},
		{"expired", "1", owner, now.Add(-time.Second), true, false},
		{"released", "1", Owner{}, time.Time{}, true, false},
		{"local", "", Owner{}, time.Time{}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &memoryStore{observation: Observation{Now: now, Revision: tc.rev, Record: Record{Version: 1, Owner: tc.owner, ExpiresAt: tc.expires}}}
			if tc.rev == "" {
				s.observation.Record = Record{}
			}
			before := s.observation
			labels := &visibilityFixture{present: tc.before}
			if err := ReconcileVisibility(t.Context(), s, labels, "42"); err != nil {
				t.Fatal(err)
			}
			if labels.present != tc.want || s.writes != 0 || s.observation != before {
				t.Fatal("visibility reconciliation altered authority or produced wrong label")
			}
		})
	}
}

func TestDelayedVisibilityCleanupRepairsSuccessorLabel(t *testing.T) {
	now := time.Now().UTC()
	s := &memoryStore{observation: Observation{Now: now, Revision: "released", Record: Record{Version: 1}}}
	labels := &visibilityFixture{present: true}
	labels.onWrite = func(present bool) {
		if !present {
			s.observation = Observation{Now: now, Revision: "successor", Record: Record{Version: 1, Owner: Owner{"next", "run", "token"}, ExpiresAt: now.Add(time.Minute)}}
		}
	}
	if err := ReconcileVisibility(t.Context(), s, labels, "42"); err != nil {
		t.Fatal(err)
	}
	if !labels.present || labels.writes != 2 || s.writes != 0 {
		t.Fatal("delayed cleanup did not restore successor visibility")
	}
}

func TestVisibilityWriteFailurePreservesOwnerAndRetries(t *testing.T) {
	s := &memoryStore{observation: Observation{Now: time.Now().UTC(), Revision: "1", Record: Record{Version: 1, Owner: Owner{"instance", "run", "token"}, ExpiresAt: time.Now().Add(time.Minute)}}}
	before := s.observation
	failed := errors.New("label unavailable")
	labels := &visibilityFixture{err: failed}
	if err := ReconcileVisibility(t.Context(), s, labels, "42"); !errors.Is(err, failed) {
		t.Fatalf("label failure lost: %v", err)
	}
	if s.observation != before || s.writes != 0 {
		t.Fatal("label failure changed ownership")
	}
	labels.err = nil
	if err := ReconcileVisibility(t.Context(), s, labels, "42"); err != nil || !labels.present {
		t.Fatalf("retry: %v", err)
	}
}
