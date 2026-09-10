package claimsclient

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

type sharedClientStore struct {
	mu       sync.Mutex
	record   sharedclaim.Record
	revision string
	writes   int
	loseACK  bool
}

func (s *sharedClientStore) Read(context.Context, string) (sharedclaim.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sharedclaim.Observation{Record: s.record, Revision: s.revision, Now: time.Now()}, nil
}

func (s *sharedClientStore) CompareAndSwap(_ context.Context, _, revision string, record sharedclaim.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != s.revision {
		return sharedclaim.ErrConflict
	}
	s.writes++
	s.record, s.revision = record, fmt.Sprint(s.writes)
	if s.loseACK {
		return errors.New("provider committed but acknowledgment was lost")
	}
	return nil
}

type sharedClientResolver struct {
	binding      SharedClaimBinding
	admissionErr error
	releaseErr   error
}

func TestSharedVisibilityHookRunsAfterTransitionsIncludingLostACK(t *testing.T) {
	store := &sharedClientStore{}
	resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "42", Owner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}}}
	var observations []sharedclaim.Owner
	resolver.binding.AfterTransition = func(ctx context.Context) {
		observed, err := store.Read(ctx, "42")
		if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, observed.Record.Owner)
	}
	file, err := NewFile(FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json"), Shared: resolver})
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "42"}
	for range 2 {
		if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err != nil || !ok {
			t.Fatalf("acquire/renew: %v %v", ok, err)
		}
	}
	store.loseACK = true
	if err := file.ReleaseScoped(t.Context(), key, "run"); err == nil {
		t.Fatal("lost ACK reported as release success")
	}
	store.loseACK = false
	if err := file.ReleaseScoped(t.Context(), key, "run"); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 4 || observations[0] != resolver.binding.Owner || observations[1] != resolver.binding.Owner || observations[2] != (sharedclaim.Owner{}) || observations[3] != (sharedclaim.Owner{}) {
		t.Fatalf("visibility did not observe transition results: %+v", observations)
	}
}

func (r *sharedClientResolver) Admission(context.Context, Key, string, string) (*SharedClaimBinding, error) {
	return &r.binding, r.admissionErr
}
func (r *sharedClientResolver) Release(context.Context, Entry) (SharedClaimBinding, error) {
	return r.binding, r.releaseErr
}

func TestFileSharedClaimAdmissionSurvivesLostACKAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	store := &sharedClientStore{loseACK: true}
	resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "repo/42", Owner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}}}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "42"}
	open := func(shared SharedClaimResolver) *File {
		t.Helper()
		file, err := NewFile(FileConfig{LedgerPath: path, Shared: shared})
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	file := open(resolver)
	if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err == nil || ok {
		t.Fatal("uncertain provider acknowledgment admitted local work")
	}
	entries, err := file.ForRunAll(t.Context(), "run")
	if err != nil || len(entries) != 0 {
		t.Fatalf("local fallback after failed admission: %+v %v", entries, err)
	}
	store.loseACK = false
	if err := file.Locked(t.Context(), "test-shared", func(held Ledger) error {
		ok, _, err := held.ClaimScoped(t.Context(), key, "run", "implement", time.Minute)
		if err == nil && !ok {
			return errors.New("same-owner reconciliation refused")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A legacy assembly cannot silently discard ownership after restart.
	if err := open(nil).ReleaseScoped(t.Context(), key, "run"); err == nil {
		t.Fatal("shared release fell back to the local-only assembly")
	}
	store.loseACK = true
	if released, err := open(resolver).ReleaseAllForRun(t.Context(), "run"); err == nil || len(released) != 0 {
		t.Fatalf("uncertain bulk release reported success: %+v %v", released, err)
	}
	entries, err = open(resolver).ForRunAll(t.Context(), "run")
	if err != nil || len(entries) != 1 || entries[0].SharedOwner != resolver.binding.Owner {
		t.Fatalf("restart lost reconciliation ownership: %+v %v", entries, err)
	}
	store.loseACK = false
	if released, err := open(resolver).ReleaseAllForRun(t.Context(), "run"); err != nil || len(released) != 1 {
		t.Fatalf("release reconciliation failed: %+v %v", released, err)
	}
}

func TestFileSharedClaimRefusesWrongOwnerAndResolverFailure(t *testing.T) {
	store := &sharedClientStore{}
	resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "repo/42", Owner: sharedclaim.Owner{Instance: "instance", Run: "other", Token: "token"}}}
	file, err := NewFile(FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json"), Shared: resolver})
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "42"}
	if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err == nil || ok || store.writes != 0 {
		t.Fatal("wrong run binding reached provider admission")
	}
	resolver.binding.Owner.Run = "run"
	resolver.admissionErr = errors.New("workflow pin is unavailable")
	if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err == nil || ok || store.writes != 0 {
		t.Fatal("unverified policy fell back to local admission")
	}
	resolver.admissionErr = nil
	if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	resolver.binding.Owner.Token = "stale"
	if err := file.ReleaseScoped(t.Context(), key, "run"); err == nil || store.writes != 1 {
		t.Fatal("wrong incarnation reached provider release")
	}
	entries, err := file.ForRunAll(t.Context(), "run")
	if err != nil || len(entries) != 1 || entries[0].SharedOwner.Token != "token" {
		t.Fatalf("failed release changed ownership: %+v %v", entries, err)
	}
}

func TestSeparateFileLedgersShareOneAdmissionConstraint(t *testing.T) {
	store := &sharedClientStore{}
	clients := make([]*File, 2)
	keys := make([]Key, 2)
	for i := range clients {
		// Distinct local namespaces and even reused run IDs must still contend
		// for the same provider item. Local ledger ownership alone cannot win.
		keys[i] = Key{Gaggle: fmt.Sprintf("gaggle-%d", i), Provider: "github", ExternalID: "42"}
		resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "repo/42",
			Owner: sharedclaim.Owner{Instance: fmt.Sprintf("instance-%d", i), Run: "run", Token: fmt.Sprintf("token-%d", i)}}}
		file, err := NewFile(FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json"), Shared: resolver})
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = file
	}
	type outcome struct {
		index int
		ok    bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for i := range clients {
		go func() {
			<-start
			ok, _, err := clients[i].ClaimScoped(t.Context(), keys[i], "run", "implement", time.Minute)
			results <- outcome{index: i, ok: ok, err: err}
		}()
	}
	close(start)
	winners := 0
	for range clients {
		result := <-results
		if result.err != nil && !errors.Is(result.err, sharedclaim.ErrHeld) && !errors.Is(result.err, sharedclaim.ErrConflict) {
			t.Fatalf("unexpected admission error: %v", result.err)
		}
		entries, err := clients[result.index].ForRunAll(t.Context(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if result.ok {
			winners++
			if len(entries) != 1 || entries[0].SharedDeadline.IsZero() {
				t.Fatal("winner lacks durable shared admission")
			}
		} else if len(entries) != 0 {
			t.Fatal("loser fell back to a local-only lease")
		}
	}
	if winners != 1 {
		t.Fatalf("admitted instances = %d, want exactly one", winners)
	}
}
