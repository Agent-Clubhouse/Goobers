package claimsclient

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
)

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestSelectFailsClosedBetweenEndpointAndToken pins the selection rule: no
// endpoint means the caller's file ledger; an endpoint with no bearer or no
// run identity is an error, never a silent fall-through to a file the pod
// does not have; endpoint + bearer + run is the plane.
func TestSelectFailsClosedBetweenEndpointAndToken(t *testing.T) {
	fileCalls := 0
	file := func() (Ledger, error) {
		fileCalls++
		return NewFile(FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json")})
	}

	ledger, err := Select(envOf(nil), file)
	if err != nil || fileCalls != 1 {
		t.Fatalf("no endpoint: ledger = %T, err = %v, file constructed %d times; want the file backend once", ledger, err, fileCalls)
	}
	if _, isFile := ledger.(*File); !isFile {
		t.Fatalf("no endpoint selected %T, want *File", ledger)
	}

	_, err = Select(envOf(map[string]string{EnvEndpoint: "http://daemon"}), file)
	if !errors.Is(err, ErrEndpointWithoutToken) {
		t.Fatalf("endpoint without token: err = %v, want ErrEndpointWithoutToken", err)
	}
	_, err = Select(envOf(map[string]string{EnvEndpoint: "http://daemon", EnvToken: "t"}), file)
	if !errors.Is(err, ErrEndpointWithoutRun) {
		t.Fatalf("endpoint without run: err = %v, want ErrEndpointWithoutRun", err)
	}
	if fileCalls != 1 {
		t.Fatalf("a refused selection constructed the file backend (%d constructions)", fileCalls)
	}

	ledger, err = Select(envOf(map[string]string{EnvEndpoint: "http://daemon/", EnvToken: "t", EnvRunID: "run-1"}), file)
	if err != nil {
		t.Fatal(err)
	}
	plane, isHTTP := ledger.(*HTTP)
	if !isHTTP {
		t.Fatalf("endpoint + token selected %T, want *HTTP", ledger)
	}
	if plane.cfg.BaseURL != "http://daemon" || plane.ContainedRunID() != "run-1" {
		t.Fatalf("plane config = %+v", plane.cfg)
	}
}

func TestListingLookupAndHistory(t *testing.T) {
	t1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t1.Add(2 * time.Hour)
	listing := Listing{
		Entries: []Entry{
			{ItemID: "7", RunID: "legacy-run"},
			{ItemID: "7", Gaggle: "g", Provider: "github", ExternalID: "7", RunID: "scoped-run"},
			{ItemID: "pr/9", Gaggle: "g", Provider: "github", ExternalID: "pr/9", RunID: "pr-run"},
		},
		History: []Entry{
			{ItemID: "7", Gaggle: "g", Provider: "github", ExternalID: "7", RunID: "a", ClaimedAt: t1, ReleasedAt: &t2},
			{ItemID: "7", Gaggle: "g", Provider: "github", ExternalID: "7", RunID: "b", ClaimedAt: t3},
			{ItemID: "8", Gaggle: "g", Provider: "github", ExternalID: "8", RunID: "c", ClaimedAt: t1, ReleasedAt: &t3},
		},
	}
	if entry, ok := listing.Lookup(Key{ExternalID: "7"}); !ok || entry.RunID != "legacy-run" {
		t.Fatalf("legacy lookup = %+v, %v; want the unscoped entry", entry, ok)
	}
	if entry, ok := listing.Lookup(Key{Gaggle: "g", Provider: "github", ExternalID: "7"}); !ok || entry.RunID != "scoped-run" {
		t.Fatalf("scoped lookup = %+v, %v; want the namespaced entry", entry, ok)
	}
	if _, ok := listing.Lookup(Key{Gaggle: "other", Provider: "github", ExternalID: "7"}); ok {
		t.Fatal("lookup in a namespace with no entry succeeded")
	}
	history := listing.HistoryForItem("7")
	if len(history) != 2 || history[0].RunID != "b" || history[1].RunID != "a" {
		t.Fatalf("history for 7 = %+v, want b (claimed t3) before a (released t2)", history)
	}
	if got := listing.HistoryForItem("nope"); len(got) != 0 {
		t.Fatalf("history for an unknown item = %+v", got)
	}
	if key := KeyForEntry(Entry{ItemID: "7"}); key != (Key{ExternalID: "7"}) {
		t.Fatalf("legacy KeyForEntry = %+v", key)
	}
	if key := KeyForEntry(listing.Entries[1]); key != (Key{Gaggle: "g", Provider: "github", ExternalID: "7"}) {
		t.Fatalf("scoped KeyForEntry = %+v", key)
	}
	if !InNamespace(Entry{ItemID: "7"}, "g", "github") || InNamespace(listing.Entries[1], "h", "github") {
		t.Fatal("InNamespace: legacy entries belong everywhere, scoped entries only to their own namespace")
	}
}

func TestLeaseSecondsRoundsUpAndClamps(t *testing.T) {
	for _, tc := range []struct {
		lease time.Duration
		want  int
	}{
		{time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{30 * time.Minute, 1800},
		{3 * time.Hour, MaxLeaseSeconds},
	} {
		got, err := leaseSeconds(tc.lease)
		if err != nil || got != tc.want {
			t.Errorf("leaseSeconds(%s) = %d, %v; want %d", tc.lease, got, err, tc.want)
		}
	}
	if _, err := leaseSeconds(0); err == nil {
		t.Fatal("a zero lease was accepted")
	}
	if _, err := leaseSeconds(-time.Second); err == nil {
		t.Fatal("a negative lease was accepted")
	}
}

// countingLock is the cmd/goobers withClaimLock stand-in: it counts
// acquisitions and records their operation labels.
type countingLock struct {
	ops  []string
	held int
}

func (l *countingLock) lock(operation string, fn func() error) error {
	l.ops = append(l.ops, operation)
	l.held++
	defer func() { l.held-- }()
	return fn()
}

func openTestLedger(t *testing.T, path string) *localscheduler.ClaimLedger {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

// TestFileBackendMatchesTheLedger is the byte-identical proof for the file
// backend: every primitive lands the ledger in the state the direct
// ClaimLedger call lands it in, legacy (unscoped) keys route to the
// ledger's legacy Claim/Release, and reads answer what the ledger answers.
func TestFileBackendMatchesTheLedger(t *testing.T) {
	ctx := context.Background()
	viaBackend := filepath.Join(t.TempDir(), "claims.json")
	viaLedger := filepath.Join(t.TempDir(), "claims.json")
	lock := &countingLock{}
	backend, err := NewFile(FileConfig{LedgerPath: viaBackend, Lock: lock.lock})
	if err != nil {
		t.Fatal(err)
	}
	direct := openTestLedger(t, viaLedger)
	scoped := Key{Gaggle: "g", Provider: "github", ExternalID: "7"}
	legacy := Key{ExternalID: "9"}

	// Acquire: first claimant wins, second is refused with the holder, the
	// same run re-claiming renews.
	if ok, holder, err := backend.ClaimScoped(ctx, scoped, "run-1", "implementation", time.Hour); err != nil || !ok || holder != "run-1" {
		t.Fatalf("backend claim = %v, %q, %v", ok, holder, err)
	}
	if ok, _, err := direct.ClaimScoped(scoped, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("direct claim = %v, %v", ok, err)
	}
	if ok, holder, err := backend.ClaimScoped(ctx, scoped, "run-2", "implementation", time.Hour); err != nil || ok || holder != "run-1" {
		t.Fatalf("backend contended claim = %v, %q, %v; want refused naming run-1", ok, holder, err)
	}
	if ok, holder, _ := direct.ClaimScoped(scoped, "run-2", "implementation", time.Hour); ok || holder != "run-1" {
		t.Fatalf("direct contended claim = %v, %q", ok, holder)
	}
	if ok, _, err := backend.ClaimScoped(ctx, scoped, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("backend idempotent re-claim = %v, %v", ok, err)
	}
	// Legacy keys route to the ledger's unscoped Claim.
	if ok, _, err := backend.ClaimScoped(ctx, legacy, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("backend legacy claim = %v, %v", ok, err)
	}
	if ok, _, err := direct.Claim("9", "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("direct legacy claim = %v, %v", ok, err)
	}
	if _, held := openTestLedger(t, viaBackend).Lookup("9"); !held {
		t.Fatal("legacy key did not land as the ledger's unscoped entry")
	}

	// Reads: ForRunAll and ListNamespace answer what the ledger answers,
	// legacy entries included in every namespace.
	held, err := backend.ForRunAll(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := direct.ForRunAll("run-1"); !sameEntries(held, want) {
		t.Fatalf("ForRunAll = %+v, want %+v", held, want)
	}
	listing, err := backend.ListNamespace(ctx, "g", "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("namespace listing = %+v, want the scoped entry and the legacy entry", listing.Entries)
	}
	if _, ok := listing.Lookup(scoped); !ok {
		t.Fatal("namespace listing lacks the scoped entry")
	}
	if _, ok := listing.Lookup(legacy); !ok {
		t.Fatal("namespace listing lacks the legacy entry the ledger holds exclusive against it")
	}
	other, err := backend.ListNamespace(ctx, "other", "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Entries) != 1 || other.Entries[0].ItemID != "9" {
		t.Fatalf("other namespace listing = %+v, want only the legacy entry", other.Entries)
	}

	// Release: legacy routes to Release, scoped to ReleaseScoped; a release
	// by a non-holder is the ledger's no-op; ReleaseAllForRun mirrors
	// ForRunAll + ReleaseEntry and reports what it surrendered.
	if err := backend.ReleaseScoped(ctx, scoped, "run-2"); err != nil {
		t.Fatal(err)
	}
	if _, stillHeld := openTestLedger(t, viaBackend).LookupScoped(scoped); !stillHeld {
		t.Fatal("a non-holder's release surrendered the claim")
	}
	released, err := backend.ReleaseAllForRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 {
		t.Fatalf("ReleaseAllForRun released %+v, want both of run-1's claims", released)
	}
	for _, entry := range direct.ForRunAll("run-1") {
		if err := direct.ReleaseEntry(entry, "run-1"); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := openTestLedger(t, viaBackend).Snapshot(), openTestLedger(t, viaLedger).Snapshot(); len(got) != 0 || len(want) != 0 {
		t.Fatalf("after release-all: backend ledger %+v, direct ledger %+v; want both empty", got, want)
	}
	// History survives the release on both.
	after, err := backend.ListNamespace(ctx, "g", "github")
	if err != nil {
		t.Fatal(err)
	}
	if got := after.HistoryForItem("7"); len(got) != 1 || got[0].ReleasedAt == nil || got[0].RunID != "run-1" {
		t.Fatalf("history for 7 after release = %+v, want one released entry for run-1", got)
	}
	if want := direct.HistoryForItem("7"); len(want) != 1 || want[0].ReleasedAt == nil {
		t.Fatalf("direct history for 7 = %+v", want)
	}

	// Bare mutations each took the lock under a default label; bare reads
	// took none.
	wantOps := []string{
		fileOperationClaim, fileOperationClaim, fileOperationClaim, fileOperationClaim,
		fileOperationRelease, fileOperationReleaseAll,
	}
	if !reflect.DeepEqual(lock.ops, wantOps) {
		t.Fatalf("lock operations = %v, want %v", lock.ops, wantOps)
	}
}

func sameEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ItemID != b[i].ItemID || a[i].RunID != b[i].RunID || a[i].Gaggle != b[i].Gaggle || a[i].Provider != b[i].Provider {
			return false
		}
	}
	return true
}

// TestFileLockedIsOneCriticalSection pins Locked's contract on the file
// backend: one lock acquisition under the caller's label, one ledger open,
// every primitive inside operates on that open ledger without re-locking
// (a nested Locked is re-entrant, not a self-deadlock), and a nil Lock
// means the caller already holds it.
func TestFileLockedIsOneCriticalSection(t *testing.T) {
	ctx := context.Background()
	lock := &countingLock{}
	opens := 0
	backend, err := NewFile(FileConfig{
		LedgerPath: filepath.Join(t.TempDir(), "claims.json"),
		Lock:       lock.lock,
		Open: func(path string, opts ...localscheduler.LedgerOption) (FileLedger, error) {
			opens++
			return localscheduler.OpenClaimLedger(path, opts...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "1"}
	err = backend.Locked(ctx, "backlog-query.claim", func(tx Ledger) error {
		if lock.held != 1 {
			t.Fatalf("lock held %d times inside Locked, want 1", lock.held)
		}
		if _, _, err := tx.ClaimScoped(ctx, key, "run-1", "w", time.Hour); err != nil {
			return err
		}
		if _, err := tx.ForRunAll(ctx, "run-1"); err != nil {
			return err
		}
		return tx.Locked(ctx, "nested", func(inner Ledger) error {
			if lock.held != 1 {
				t.Fatalf("nested Locked re-acquired the lock (held %d)", lock.held)
			}
			return inner.ReleaseScoped(ctx, key, "run-1")
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lock.ops, []string{"backlog-query.claim"}) || opens != 1 {
		t.Fatalf("lock ops = %v, opens = %d; want one labelled acquisition and one open", lock.ops, opens)
	}

	held, err := NewFile(FileConfig{LedgerPath: backend.cfg.LedgerPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Locked(ctx, "already-held", func(tx Ledger) error {
		_, _, err := tx.ClaimScoped(ctx, key, "run-2", "w", time.Hour)
		return err
	}); err != nil {
		t.Fatalf("nil-Lock backend: %v", err)
	}
	if err := held.ReleaseScoped(ctx, key, "run-2"); err != nil {
		t.Fatalf("nil-Lock bare primitive: %v", err)
	}
}

func TestFileMergeLockIsTheInjectedFlock(t *testing.T) {
	ctx := context.Background()
	without, err := NewFile(FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := without.MergeLock(ctx, MergeLock{}, func() error { return nil }); err == nil {
		t.Fatal("MergeLock with no flock configured ran the window")
	}
	flocked := 0
	with, err := NewFile(FileConfig{
		LedgerPath: filepath.Join(t.TempDir(), "claims.json"),
		MergeLock: func(fn func() error) error {
			flocked++
			return fn()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := with.MergeLock(ctx, MergeLock{}, func() error { ran = true; return nil }); err != nil || !ran || flocked != 1 {
		t.Fatalf("MergeLock: err = %v, ran = %v, flocked = %d", err, ran, flocked)
	}
	if err := with.Locked(ctx, "op", func(tx Ledger) error {
		return tx.MergeLock(ctx, MergeLock{}, func() error { return nil })
	}); err != nil || flocked != 2 {
		t.Fatalf("MergeLock inside Locked: err = %v, flocked = %d", err, flocked)
	}
}

func TestMergeLockKey(t *testing.T) {
	key := MergeLockKey("g", "github", "acme", "web")
	if key != (Key{Gaggle: "g", Provider: "github", ExternalID: "merge-lock/acme/web"}) {
		t.Fatalf("MergeLockKey = %+v", key)
	}
}
