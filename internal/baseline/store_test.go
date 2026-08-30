package baseline

import (
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "baseline.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store, path
}

func redObservation(baseSHA string) Observation {
	signature := "assert failed"
	return Observation{
		Repo:        "acme/web",
		BaseSHA:     baseSHA,
		Command:     CommandKey([]string{"make", "ci"}),
		Signature:   signature,
		Fingerprint: Fingerprint([]string{"make", "ci"}, signature),
		ObservedAt:  time.Date(2026, 8, 30, 7, 0, 0, 0, time.UTC),
	}
}

func TestStoreRoundTripsAcrossProcesses(t *testing.T) {
	store, path := newStore(t)
	observation := redObservation("sha-1")
	if err := store.Record(observation); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := store.Park(observation, Waiter{Subject: "101", RunID: "run-1", BaseSHA: "sha-1"}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Baseline("acme/web", "sha-1", []string{"make", "ci"})
	if !ok || got.Fingerprint != observation.Fingerprint {
		t.Fatalf("baseline after reopen = %+v ok=%v, want the recorded observation", got, ok)
	}
	blockers := reopened.Blockers("acme/web")
	if len(blockers) != 1 || len(blockers[0].Waiting) != 1 {
		t.Fatalf("blockers after reopen = %+v, want one blocker with one waiter", blockers)
	}
}

func TestParkIsIdempotentPerSubject(t *testing.T) {
	store, _ := newStore(t)
	observation := redObservation("sha-1")

	for range 3 {
		if _, err := store.Park(observation, Waiter{Subject: "101", RunID: "run-1", BaseSHA: "sha-1"}); err != nil {
			t.Fatalf("Park: %v", err)
		}
	}
	blockers := store.Blockers("acme/web")
	if len(blockers) != 1 {
		t.Fatalf("blockers = %d, want exactly 1 durable shared blocker", len(blockers))
	}
	if len(blockers[0].Waiting) != 1 {
		t.Fatalf("waiting = %d, want 1: re-parking the same subject refreshes it", len(blockers[0].Waiting))
	}
}

func TestReadyToRetryReleasesOnGreenBaseline(t *testing.T) {
	store, _ := newStore(t)
	observation := redObservation("sha-1")
	if _, err := store.Park(observation, Waiter{Subject: "101", BaseSHA: "sha-1"}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if ready := store.ReadyToRetry("acme/web", "sha-1"); len(ready) != 0 {
		t.Fatalf("ready = %+v, want none while the same base is still red", ready)
	}

	green := observation
	green.Green, green.Fingerprint, green.Signature = true, "", ""
	if err := store.Record(green); err != nil {
		t.Fatalf("Record green: %v", err)
	}
	ready := store.ReadyToRetry("acme/web", "sha-1")
	if len(ready) != 1 || ready[0].Subject != "101" {
		t.Fatalf("ready = %+v, want the parked subject released once the baseline is green", ready)
	}
}

func TestReadyToRetryReleasesWhenTheBaseAdvances(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Park(redObservation("sha-1"), Waiter{Subject: "101", BaseSHA: "sha-1"}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	ready := store.ReadyToRetry("acme/web", "sha-2")
	if len(ready) != 1 || ready[0].Subject != "101" {
		t.Fatalf("ready = %+v, want the parked subject retried against the advanced base", ready)
	}
}

func TestReleaseDropsARetriedSubject(t *testing.T) {
	store, _ := newStore(t)
	observation := redObservation("sha-1")
	if _, err := store.Park(observation, Waiter{Subject: "101", BaseSHA: "sha-1"}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if _, err := store.Park(observation, Waiter{Subject: "202", BaseSHA: "sha-1"}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := store.Release("acme/web", "101"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	blockers := store.Blockers("acme/web")
	if len(blockers) != 1 || len(blockers[0].Waiting) != 1 || blockers[0].Waiting[0].Subject != "202" {
		t.Fatalf("blockers = %+v, want only the still-parked subject", blockers)
	}
}

func TestRedBaseAtANewShaReusesTheSameBlocker(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Park(redObservation("sha-1"), Waiter{Subject: "101", BaseSHA: "sha-1"}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	blocker, err := store.Park(redObservation("sha-2"), Waiter{Subject: "202", BaseSHA: "sha-2"})
	if err != nil {
		t.Fatalf("Park: %v", err)
	}

	if len(store.Blockers("acme/web")) != 1 {
		t.Fatalf("blockers = %d, want one blocker per distinct failure, not per base SHA", len(store.Blockers("acme/web")))
	}
	if len(blocker.BaseSHAs) != 2 {
		t.Fatalf("baseShas = %v, want both red bases recorded", blocker.BaseSHAs)
	}
}

func TestNilStoreIsInert(t *testing.T) {
	var store *Store
	if _, ok := store.Baseline("acme/web", "sha", []string{"make"}); ok {
		t.Fatal("Baseline ok = true, want false on a nil store")
	}
	if store.Blockers("") != nil || store.ReadyToRetry("acme/web", "sha") != nil {
		t.Fatal("want nil results from a nil store")
	}
	if err := store.Record(Observation{}); err == nil {
		t.Fatal("Record error = nil, want ErrNoStore")
	}
}

// TestStaleGreenObservationDoesNotResolveABlocker guards the release gate: a
// green measurement that predates the blocker's last red sighting, or that is
// pinned to a base the blocker has already moved past, is evidence about a
// commit nobody is waiting on. Resolving on it would release every waiter onto
// a base that still reproduces the failure.
func TestStaleGreenObservationDoesNotResolveABlocker(t *testing.T) {
	store, _ := newStore(t)
	parkedAt := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	if _, err := store.Park(redObservation("sha-1"), Waiter{Subject: "101", BaseSHA: "sha-1", ParkedAt: parkedAt}); err != nil {
		t.Fatalf("Park at sha-1: %v", err)
	}
	if _, err := store.Park(redObservation("sha-2"), Waiter{Subject: "202", BaseSHA: "sha-2", ParkedAt: parkedAt.Add(time.Hour)}); err != nil {
		t.Fatalf("Park at sha-2: %v", err)
	}

	early := redObservation("sha-2")
	early.Green, early.Fingerprint, early.Signature = true, "", ""
	early.ObservedAt = parkedAt
	if err := store.Record(early); err != nil {
		t.Fatalf("Record early green: %v", err)
	}
	if parkedOnCurrentBase(store.ReadyToRetry("acme/web", "sha-2")) {
		t.Fatal("subject 202 released: a green measurement predating the last red sighting must not resolve the blocker")
	}

	superseded := redObservation("sha-1")
	superseded.Green, superseded.Fingerprint, superseded.Signature = true, "", ""
	superseded.ObservedAt = parkedAt.Add(2 * time.Hour)
	if err := store.Record(superseded); err != nil {
		t.Fatalf("Record superseded green: %v", err)
	}
	if parkedOnCurrentBase(store.ReadyToRetry("acme/web", "sha-2")) {
		t.Fatal("subject 202 released: sha-1 is a base the blocker has already moved past")
	}

	current := redObservation("sha-2")
	current.Green, current.Fingerprint, current.Signature = true, "", ""
	current.ObservedAt = parkedAt.Add(3 * time.Hour)
	if err := store.Record(current); err != nil {
		t.Fatalf("Record current green: %v", err)
	}
	if !parkedOnCurrentBase(store.ReadyToRetry("acme/web", "sha-2")) {
		t.Fatal("subject 202 still parked: a green measurement of the current base resolves the blocker")
	}
}

// parkedOnCurrentBase reports whether subject 202 — the waiter pinned to the
// blocker's current base — is among the released waiters. Subject 101 is parked
// at the older base and is always retry-ready there, so only 202 isolates the
// resolution gate.
func parkedOnCurrentBase(ready []Waiter) bool {
	for _, waiter := range ready {
		if waiter.Subject == "202" {
			return true
		}
	}
	return false
}
