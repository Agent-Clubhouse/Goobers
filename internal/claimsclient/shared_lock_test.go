package claimsclient

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type guardedSharedStore struct {
	sharedClientStore
	check   func() error
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *guardedSharedStore) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	if err := s.check(); err != nil {
		return sharedclaim.Observation{}, err
	}
	if s.entered != nil {
		s.once.Do(func() { close(s.entered); <-s.resume })
	}
	return s.sharedClientStore.Read(ctx, key)
}
func (s *guardedSharedStore) CompareAndSwap(ctx context.Context, key, rev string, record sharedclaim.Record) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.sharedClientStore.CompareAndSwap(ctx, key, rev, record)
}

func TestSharedCallbacksAndProviderNeverRunUnderClaimsLock(t *testing.T) {
	var held atomic.Bool
	check := func() error {
		if held.Load() {
			return fmt.Errorf("provider or hook entered under claims lock")
		}
		return nil
	}
	store := &guardedSharedStore{check: check}
	resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "42", Owner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}, BeforeTransition: func(context.Context) error { return check() }, AfterTransition: func(context.Context) {
		if err := check(); err != nil {
			t.Error(err)
		}
	}}}
	cfg := sharedTestFileConfig(filepath.Join(t.TempDir(), "claims.json"), resolver)
	original := cfg.Lock
	cfg.Lock = func(op string, fn func() error) error {
		return original(op, func() error { held.Store(true); defer held.Store(false); return fn() })
	}
	file, err := NewFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "42"}
	if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	if err := file.ReleaseScoped(t.Context(), key, "run", claimrelease.Completed.Reason()); err != nil {
		t.Fatal(err)
	}
	if err := file.Locked(t.Context(), "test-held-refusal", func(tx Ledger) error {
		if _, _, err := tx.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err != ErrSharedRequiresUnlocked {
			return fmt.Errorf("held shared admission did not fail closed: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminRevokesPendingSharedAdmissionBeforeWaitingForCleanup(t *testing.T) {
	store := &guardedSharedStore{check: func() error { return nil }, entered: make(chan struct{}), resume: make(chan struct{})}
	var unblock sync.Once
	defer unblock.Do(func() { close(store.resume) })
	resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "42", Owner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}}}
	cfg := sharedTestFileConfig(filepath.Join(t.TempDir(), "claims.json"), resolver)
	file, err := NewFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "42"}
	admitted := make(chan error, 1)
	go func() {
		ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute)
		if ok {
			err = fmt.Errorf("revoked admission succeeded")
		}
		admitted <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("admission never reached provider")
	}
	entries, err := file.ForRunAll(t.Context(), "run")
	if err != nil || len(entries) != 1 {
		t.Fatalf("reservation: %+v %v", entries, err)
	}
	revoked := make(chan struct{})
	original := cfg.Lock
	cfg.Lock = func(op string, fn func() error) error {
		err := original(op, fn)
		if op == "claims.shared.revoke" {
			close(revoked)
		}
		return err
	}
	admin, err := NewFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := make(chan error, 1)
	go func() { cleaned <- admin.ForceReleaseExpected(t.Context(), entries[0], "cli") }()
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("admin waited for provider before revoking")
	}
	reopened, err := localscheduler.OpenClaimLedger(cfg.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	current, held := reopened.LookupScoped(key)
	if !held || !current.SharedRevoked || current.ExpiresAt.After(time.Now()) {
		t.Fatalf("revocation not durable: %+v", current)
	}
	unblock.Do(func() { close(store.resume) })
	select {
	case err := <-admitted:
		if err == nil {
			t.Fatal("late remote acknowledgment ignored revocation")
		}
	case <-time.After(time.Second):
		t.Fatal("admission did not finish")
	}
	select {
	case err := <-cleaned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("admin cleanup deadlocked")
	}
	reopened, err = localscheduler.OpenClaimLedger(cfg.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.LookupScoped(key); held {
		t.Fatal("cleanup retained reservation")
	}
	if ok, _, err := file.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err == nil || ok {
		t.Fatal("revoked run was readmitted")
	}
}
