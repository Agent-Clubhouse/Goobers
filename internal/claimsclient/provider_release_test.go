package claimsclient

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
)

func TestProviderReleaseRetriesAndSerializesCleanup(t *testing.T) {
	var claimsLock, cleanupLock sync.Mutex
	var held atomic.Bool
	client, err := NewFile(FileConfig{
		LedgerPath: filepath.Join(t.TempDir(), "claims.json"),
		Lock: func(_ string, fn func() error) error {
			claimsLock.Lock()
			defer claimsLock.Unlock()
			held.Store(true)
			defer held.Store(false)
			return fn()
		},
		ProviderReleaseLock: func(_ Key, fn func() error) error {
			cleanupLock.Lock()
			defer cleanupLock.Unlock()
			return fn()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := Key{ExternalID: "7"}
	if ok, _, err := client.ClaimScoped(ctx, key, "run", "curation", time.Minute); !ok || err != nil {
		t.Fatalf("claim: %t %v", ok, err)
	}
	entries, err := client.ForRunAll(ctx, "run")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	entry := entries[0]
	unavailable := errors.New("provider unavailable")
	if ok, err := client.ReleaseWithProvider(ctx, entry, claimrelease.Completed.Reason(), func(context.Context) error {
		if held.Load() {
			t.Error("provider called under claims lock")
		}
		return unavailable
	}); ok || !errors.Is(err, unavailable) {
		t.Fatalf("failed cleanup: %t %v", ok, err)
	}
	entries, _ = client.ForRunAll(ctx, "run")
	if len(entries) != 1 || !entries[0].ReleasePending {
		t.Fatalf("failed cleanup lost custody: %v", entries)
	}
	started, proceed := make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	var calls atomic.Int32
	cleanup := func(context.Context) error {
		if held.Load() {
			return errors.New("provider called under claims lock")
		}
		if calls.Add(1) == 1 {
			close(started)
			<-proceed
		}
		return nil
	}
	release := func() {
		_, err := client.ReleaseWithProvider(ctx, entry, claimrelease.Completed.Reason(), cleanup)
		results <- err
	}
	go release()
	<-started
	go release()
	close(proceed)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("stale retry repeated provider I/O: %d", calls.Load())
	}
	if ok, _, err := client.ClaimScoped(ctx, key, "successor", "implementation", time.Minute); !ok || err != nil {
		t.Fatalf("next owner: %t %v", ok, err)
	}
	if ok, err := client.ReleaseWithProvider(ctx, entry, claimrelease.Completed.Reason(), cleanup); ok || err != nil {
		t.Fatalf("stale snapshot: %t %v", ok, err)
	}
	if calls.Load() != 1 {
		t.Fatal("stale cleanup reached provider after a successor acquired")
	}
}
