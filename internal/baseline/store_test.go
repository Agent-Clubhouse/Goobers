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

func TestAttachExternalRefRecordsTheDurableBlocker(t *testing.T) {
	store, _ := newStore(t)
	blocker, err := store.Park(redObservation("sha-1"), Waiter{Subject: "101", BaseSHA: "sha-1"})
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := store.AttachExternalRef(blocker.Key, "https://example.test/issues/9"); err != nil {
		t.Fatalf("AttachExternalRef: %v", err)
	}
	if err := store.AttachExternalRef("no-such-blocker", "x"); err == nil {
		t.Fatal("AttachExternalRef error = nil, want an error for an unknown blocker")
	}

	blockers := store.Blockers("")
	if len(blockers) != 1 || blockers[0].ExternalRef != "https://example.test/issues/9" {
		t.Fatalf("blockers = %+v, want the external blocker reference recorded", blockers)
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
