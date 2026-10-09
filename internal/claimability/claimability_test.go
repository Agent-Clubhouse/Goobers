package claimability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// providerStore is an in-memory shared lease store with its own provider
// clock, independent of the worker clock passed to Observe.
type providerStore struct {
	mu       sync.Mutex
	clock    time.Time
	records  map[string]sharedclaim.Record
	revision map[string]string
	reads    int
	writes   int
	fail     map[string]error
}

func newProviderStore(clock time.Time) *providerStore {
	return &providerStore{clock: clock, records: map[string]sharedclaim.Record{}, revision: map[string]string{}, fail: map[string]error{}}
}

func (s *providerStore) Read(_ context.Context, key string) (sharedclaim.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if err := s.fail[key]; err != nil {
		return sharedclaim.Observation{}, err
	}
	return sharedclaim.Observation{Record: s.records[key], Revision: s.revision[key], Now: s.clock}, nil
}

func (s *providerStore) CompareAndSwap(_ context.Context, key, revision string, record sharedclaim.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision[key] != revision {
		return sharedclaim.ErrConflict
	}
	s.writes++
	s.records[key], s.revision[key] = record, fmt.Sprint(s.writes)
	return nil
}

var testLimits = Limits{MaxCandidates: 10, MaxSharedReads: 10, Timeout: time.Second, DisagreementWindow: time.Hour}

func localSource(snapshot LocalSnapshot) func(context.Context) (LocalSnapshot, error) {
	return func(context.Context) (LocalSnapshot, error) { return snapshot, nil }
}

func blockedSource(snapshot BlockedSnapshot) func(context.Context) (BlockedSnapshot, error) {
	return func(context.Context) (BlockedSnapshot, error) { return snapshot, nil }
}

func classifyOne(t *testing.T, now time.Time, policy Policy, candidate Candidate, sources Sources) Verdict {
	t.Helper()
	result := Observe(t.Context(), now, policy, []Candidate{candidate}, false, sources, testLimits)
	switch {
	case result.Available == 1:
		return Available
	case result.Held == 1:
		return Held
	case result.Waiting == 1:
		return Waiting
	case result.Unknown == 1:
		return Unknown
	}
	t.Fatalf("unclassified: %+v", result)
	return ""
}

func TestObserveClassifiesLocalAdmissionEvidenceWithFakeClock(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	policy := Policy{Gaggle: "g", Provider: "github"}
	live := now.Add(time.Minute)
	local := LocalSnapshot{
		Entries: []localscheduler.ClaimEntry{
			{ItemID: "held", Gaggle: "g", Provider: "github", ExternalID: "held", RunID: "r", ExpiresAt: live},
			{ItemID: "expired", Gaggle: "g", Provider: "github", ExternalID: "expired", RunID: "r", ExpiresAt: now},
			{ItemID: "legacy", RunID: "r", ExpiresAt: live},
			{ItemID: "elsewhere", Gaggle: "other", Provider: "github", ExternalID: "elsewhere", RunID: "r", ExpiresAt: live},
		},
		History: []localscheduler.ClaimEntry{
			{ItemID: "backoff", Gaggle: "g", Provider: "github", ExternalID: "backoff", Verification: localscheduler.ClaimVerification{State: "contended", ProviderRunID: "p", ObservedAt: now.Add(-59 * time.Minute)}},
			{ItemID: "lapsed", Gaggle: "g", Provider: "github", ExternalID: "lapsed", Verification: localscheduler.ClaimVerification{State: "ownership-mismatch", ProviderRunID: "p", ObservedAt: now.Add(-time.Hour)}},
		},
	}
	blocked := BlockedSnapshot{Scoped: map[string]struct{}{"blocked": {}}, Unscoped: map[string]struct{}{"unscoped": {}}}
	sources := Sources{Local: localSource(local), Blocked: blockedSource(blocked)}
	for id, want := range map[string]Verdict{
		"free": Available, "held": Held, "expired": Unknown, "legacy": Held, "elsewhere": Available,
		"backoff": Waiting, "lapsed": Available, "blocked": Unknown, "unscoped": Unknown,
	} {
		if got := classifyOne(t, now, policy, Candidate{ID: id}, sources); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	// A backoff past the shortest admissible lease but inside the longest may
	// already admit under the shorter one: neither waiting nor available.
	mixed := testLimits
	mixed.DisagreementWindow, mixed.DisagreementMaxWindow = 30*time.Minute, time.Hour
	if got := Observe(t.Context(), now, policy, []Candidate{{ID: "backoff"}}, false, sources, mixed); got.Unknown != 1 || got.Complete {
		t.Errorf("backoff between lease windows = %+v, want unknown", got)
	}
	if got := classifyOne(t, now, policy, Candidate{ID: "free", ProviderClaimed: true}, sources); got != Unknown {
		t.Errorf("provider-claimed marker = %s, want unknown", got)
	}
	// An ungaggled stage claims legacy keys and has no disagreement backoff.
	legacyPolicy := Policy{}
	if got := classifyOne(t, now, legacyPolicy, Candidate{ID: "held"}, sources); got != Available {
		t.Errorf("legacy policy honoured a scoped lease: %s", got)
	}
	if got := classifyOne(t, now, legacyPolicy, Candidate{ID: "legacy"}, sources); got != Held {
		t.Errorf("legacy policy ignored a legacy lease: %s", got)
	}
}

func TestObserveSharedLeaseUsesProviderClockAndStaysReadOnly(t *testing.T) {
	workerNow := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// The provider clock is deliberately far from the worker clock.
	store := newProviderStore(workerNow.Add(-3 * time.Hour))
	other := sharedclaim.Owner{Instance: "other-deployment", Run: "run", Token: "token"}
	store.records["held"], store.revision["held"] = sharedclaim.Record{Version: 1, Owner: other, ExpiresAt: store.clock.Add(time.Minute)}, "1"
	store.records["lapsed"], store.revision["lapsed"] = sharedclaim.Record{Version: 1, Owner: other, ExpiresAt: store.clock}, "2"
	store.records["released"], store.revision["released"] = sharedclaim.Record{Version: 1}, "3"
	store.records["corrupt"], store.revision["corrupt"] = sharedclaim.Record{Version: 9}, "4"
	store.fail["unreadable"] = errors.New("provider unavailable")
	sources := Sources{Local: localSource(LocalSnapshot{}), Blocked: blockedSource(BlockedSnapshot{}), Shared: func(context.Context) (sharedclaim.Store, error) { return store, nil }}
	policy := Policy{Gaggle: "g", Provider: "github", Shared: true}
	for id, want := range map[string]Verdict{
		"absent": Available, "released": Available, "held": Held, "lapsed": Unknown, "corrupt": Unknown, "unreadable": Unknown,
	} {
		if got := classifyOne(t, workerNow, policy, Candidate{ID: id}, sources); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	if store.writes != 0 {
		t.Fatalf("observation wrote %d shared records", store.writes)
	}
	unavailable := Sources{Local: localSource(LocalSnapshot{}), Blocked: blockedSource(BlockedSnapshot{}), Shared: func(context.Context) (sharedclaim.Store, error) { return nil, errors.New("no credentials") }}
	if got := classifyOne(t, workerNow, policy, Candidate{ID: "absent"}, unavailable); got != Unknown {
		t.Fatalf("unavailable shared store = %s", got)
	}
	if got := classifyOne(t, workerNow, policy, Candidate{ID: "absent"}, Sources{Local: sources.Local, Blocked: sources.Blocked}); got != Unknown {
		t.Fatalf("missing shared store = %s", got)
	}
	noClock := &providerStore{records: map[string]sharedclaim.Record{}, revision: map[string]string{}, fail: map[string]error{}}
	if got := classifyOne(t, workerNow, policy, Candidate{ID: "absent"}, Sources{Local: sources.Local, Blocked: sources.Blocked, Shared: func(context.Context) (sharedclaim.Store, error) { return noClock, nil }}); got != Unknown {
		t.Fatalf("missing provider clock = %s", got)
	}
}

func TestObserveBoundsAndFailuresPreserveUnknown(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	candidates := []Candidate{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	ok := Sources{Local: localSource(LocalSnapshot{}), Blocked: blockedSource(BlockedSnapshot{})}
	if got := Observe(t.Context(), now, Policy{}, candidates, false, ok, testLimits); !got.Complete || got.Available != 3 || got.Observed != 3 {
		t.Fatalf("complete local observation: %+v", got)
	}
	if got := Observe(t.Context(), now, Policy{}, candidates, true, ok, testLimits); got.Complete || got.Available != 3 {
		t.Fatalf("truncated poll was complete: %+v", got)
	}
	limited := testLimits
	limited.MaxCandidates = 2
	if got := Observe(t.Context(), now, Policy{}, candidates, false, ok, limited); got.Complete || got.Observed != 2 {
		t.Fatalf("candidate bound: %+v", got)
	}
	store := newProviderStore(now)
	shared := Sources{Local: ok.Local, Blocked: ok.Blocked, Shared: func(context.Context) (sharedclaim.Store, error) { return store, nil }}
	limited = testLimits
	limited.MaxSharedReads = 1
	if got := Observe(t.Context(), now, Policy{Gaggle: "g", Provider: "github", Shared: true}, candidates, false, shared, limited); got.Available != 1 || got.Unknown != 2 || got.Complete || store.reads != 1 {
		t.Fatalf("shared read bound: %+v reads=%d", got, store.reads)
	}
	failing := errors.New("unreadable")
	for name, sources := range map[string]Sources{
		"ledger":  {Local: func(context.Context) (LocalSnapshot, error) { return LocalSnapshot{}, failing }, Blocked: ok.Blocked},
		"blocked": {Local: ok.Local, Blocked: func(context.Context) (BlockedSnapshot, error) { return BlockedSnapshot{}, failing }},
		"missing": {},
	} {
		if got := Observe(t.Context(), now, Policy{}, candidates, false, sources, testLimits); got.Unknown != 3 || got.Complete {
			t.Errorf("%s source failure: %+v", name, got)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if got := Observe(cancelled, now, Policy{}, candidates, false, ok, testLimits); got.Unknown != 3 || got.Complete {
		t.Fatalf("cancelled observation: %+v", got)
	}
	// Cancellation mid-observation stops further shared reads.
	ctx, stop := context.WithCancel(t.Context())
	cancelling := Sources{Local: ok.Local, Blocked: ok.Blocked, Shared: func(context.Context) (sharedclaim.Store, error) {
		return cancelOnRead{store: store, cancel: stop}, nil
	}}
	if got := Observe(ctx, now, Policy{Gaggle: "g", Provider: "github", Shared: true}, candidates, false, cancelling, testLimits); got.Available != 0 || got.Unknown != 3 || got.Complete {
		t.Fatalf("mid-observation cancellation: %+v", got)
	}
}

// TestObserveSourceIgnoringDeadlineIsUnknownAtTimeout proves an accessor that
// ignores its context cannot stretch the observation past its bound.
func TestObserveSourceIgnoringDeadlineIsUnknownAtTimeout(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	defer close(release)
	slow := func(context.Context) (LocalSnapshot, error) {
		<-release
		return LocalSnapshot{}, nil
	}
	limits := testLimits
	limits.Timeout = 20 * time.Millisecond
	done := make(chan Result, 1)
	go func() {
		done <- Observe(t.Context(), now, Policy{}, []Candidate{{ID: "1"}}, false, Sources{Local: slow, Blocked: blockedSource(BlockedSnapshot{})}, limits)
	}()
	select {
	case got := <-done:
		if got.Unknown != 1 || got.Complete {
			t.Fatalf("over-time source: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("observation waited on a source past its deadline")
	}
}

func TestReadFileBoundsBytesAndDeadline(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const limit = 3*readChunkBytes + 7
	if data, err := ReadFile(t.Context(), write("fits", limit), limit); err != nil || len(data) != limit {
		t.Fatalf("at cap: %d %v", len(data), err)
	}
	if _, err := ReadFile(t.Context(), write("over", limit+1), limit); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("over cap: %v", err)
	}
	if data, err := ReadFile(t.Context(), filepath.Join(dir, "missing"), limit); err != nil || data != nil {
		t.Fatalf("missing: %q %v", data, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadFile(cancelled, write("late", 10), limit); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired deadline: %v", err)
	}
}

type cancelOnRead struct {
	store  sharedclaim.Store
	cancel context.CancelFunc
}

func (s cancelOnRead) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	s.cancel()
	return s.store.Read(ctx, key)
}

func (s cancelOnRead) CompareAndSwap(context.Context, string, string, sharedclaim.Record) error {
	return errors.New("observation must not write")
}

// TestObserveAgreesWithRealLocalAndSharedAdmission drives the real claim
// ledger and shared lease transitions: observation leaves both untouched,
// and admission then accepts exactly what was verified available and refuses
// what was observed held.
func TestObserveAgreesWithRealLocalAndSharedAdmission(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := localscheduler.OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	key := func(id string) localscheduler.ClaimKey {
		return localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: id}
	}
	if ok, _, err := ledger.ClaimScoped(key("local-held"), "holder", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed local lease: %v %v", ok, err)
	}
	store := newProviderStore(now)
	other := sharedclaim.Owner{Instance: "other-deployment", Run: "remote-run", Token: "token"}
	if err := sharedclaim.Acquire(t.Context(), store, "shared-held", other, time.Hour); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writes := store.writes
	sources := Sources{
		Local: func(context.Context) (LocalSnapshot, error) {
			reader, err := localscheduler.OpenClaimLedger(path)
			if err != nil {
				return LocalSnapshot{}, err
			}
			return LocalSnapshot{Entries: reader.Snapshot(), History: reader.HistorySnapshot()}, nil
		},
		Blocked: blockedSource(BlockedSnapshot{}),
		Shared:  func(context.Context) (sharedclaim.Store, error) { return store, nil },
	}
	policy := Policy{Gaggle: "g", Provider: "github", Shared: true}
	verdicts := map[string]Verdict{}
	for _, id := range []string{"free", "local-held", "shared-held"} {
		verdicts[id] = classifyOne(t, time.Now().UTC(), policy, Candidate{ID: id}, sources)
	}
	if verdicts["free"] != Available || verdicts["local-held"] != Held || verdicts["shared-held"] != Held {
		t.Fatalf("verdicts = %v", verdicts)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || store.writes != writes {
		t.Fatal("observation mutated the claim ledger or shared store")
	}
	self := func(id string) sharedclaim.Owner {
		return sharedclaim.Owner{Instance: "this-deployment", Run: "run-" + id, Token: "token-" + id}
	}
	for id, verdict := range verdicts {
		ok, _, err := ledger.ClaimSharedScoped(t.Context(), store, id, key(id), self(id), "implement", time.Hour)
		admitted := err == nil && ok
		if admitted != (verdict == Available) {
			t.Errorf("%s observed %s but admission admitted=%v err=%v", id, verdict, admitted, err)
		}
	}
}
