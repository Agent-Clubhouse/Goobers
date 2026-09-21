package configgeneration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"

	"github.com/goobers/goobers/internal/platform/lock"
)

func TestStoreBoundsRetainedGenerationsAndProtectsRunPins(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeFixture(t, source, "config/workflow.yaml", "initial")
	blobs, err := blobstore.NewDir(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Root: filepath.Join(root, "generations"), Blobs: blobs}
	protected := map[string]bool{}
	var lastData []byte
	var lastDigest string
	for i := 0; i < MaxGenerations; i++ {
		writeFixture(t, source, "config/workflow.yaml", fmt.Sprintf("generation: %d", i))
		data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Keep(t.Context(), data, digest, protected); err != nil {
			t.Fatal(err)
		}
		protected[digest] = true
		lastData, lastDigest = data, digest
	}
	writeFixture(t, source, "config/workflow.yaml", "generation: overflow")
	data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Keep(t.Context(), data, digest, protected); err == nil {
		t.Fatal("evicted a protected run generation")
	}
	if _, err := store.Keep(t.Context(), lastData, lastDigest, protected); err != nil {
		t.Fatalf("capacity refused existing pin: %v", err)
	}
	delete(protected, lastDigest)
	if _, err := store.Keep(t.Context(), data, digest, protected); err != nil {
		t.Fatal(err)
	}
	if ok, err := blobs.Has(t.Context(), lastDigest); err != nil || ok {
		t.Fatalf("unreferenced archive not pruned: %v %v", ok, err)
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	if count != MaxGenerations {
		t.Fatalf("retained %d generations want %d", count, MaxGenerations)
	}
	for pin := range protected {
		if ok, err := blobs.Has(t.Context(), pin); err != nil || !ok {
			t.Fatalf("protected archive missing: %s %v", pin, err)
		}
	}
}

func TestStoreRefusesCorruptRetainedGeneration(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeFixture(t, source, "config/workflow.yaml", "old")
	blobs, err := blobstore.NewDir(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Root: filepath.Join(root, "generations"), Blobs: blobs}
	data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := store.Keep(t.Context(), data, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Dir(configDir), "config/workflow.yaml", "corrupt")
	if _, err := store.Load(t.Context(), digest); err == nil {
		t.Fatal("loaded corrupt pin")
	}
	if _, err := store.Keep(t.Context(), data, digest, nil); err == nil {
		t.Fatal("accepted corrupt cache")
	}
}

func TestStoreLeasesProtectConcurrentCacheReadersAtCapacity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	store := Store{Root: filepath.Join(root, "cache"), LocalCache: true}
	var leases []*lock.Handle
	t.Cleanup(func() {
		for _, lease := range leases {
			_ = lease.Release()
		}
	})
	var evict string
	for i := 0; i < MaxGenerations; i++ {
		writeFixture(t, source, "config/workflow.yaml", fmt.Sprintf("generation: %d", i))
		data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
		if err != nil {
			t.Fatal(err)
		}
		_, lease, err := store.KeepAndAcquire(t.Context(), data, digest, nil)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		evict = digest
	}
	writeFixture(t, source, "config/workflow.yaml", "generation: beyond capacity")
	data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
	if err != nil {
		t.Fatal(err)
	}
	// A separate Store has no knowledge of the first caller's in-memory pins.
	competitor := Store{Root: store.Root, LocalCache: true}
	if _, err := competitor.Keep(t.Context(), data, digest, nil); err == nil {
		t.Fatal("evicted a leased generation")
	}
	if err := leases[len(leases)-1].Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := competitor.Keep(t.Context(), data, digest, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(t.Context(), evict); !os.IsNotExist(err) {
		t.Fatalf("released generation not reclaimed: %v", err)
	}
}

func TestStoreChecksDurableOwnerAfterObtainingPruneLease(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	protected := make(map[string]bool)
	store := Store{Root: filepath.Join(root, "cache"), LocalCache: true}
	for i := 0; i < MaxGenerations; i++ {
		writeFixture(t, source, "config/workflow.yaml", fmt.Sprintf("generation: %d", i))
		data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Keep(t.Context(), data, digest, nil); err != nil {
			t.Fatal(err)
		}
		protected[digest] = true
	}
	calls := 0
	store.DurablePins = func(context.Context) (map[string]bool, error) { calls++; return protected, nil }
	writeFixture(t, source, "config/workflow.yaml", "generation: overflow")
	data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Keep(t.Context(), data, digest, nil); err == nil {
		t.Fatal("ignored durable run pins")
	}
	if calls != MaxGenerations {
		t.Fatalf("checked %d durable owners want %d", calls, MaxGenerations)
	}
}

// Standalone engine histories have no local journal retirement signal. A fresh
// process must retain their archives and refuse overflow, even after lease loss.
func TestStoreExternalHistoryPinsSurviveRestartAndPruning(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	blobs, err := blobstore.NewDir(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Root: filepath.Join(root, "generations"), Blobs: blobs}
	pins := make([]string, 0, MaxGenerations)
	for i := 0; i < MaxGenerations; i++ {
		writeFixture(t, source, "config/workflow.yaml", fmt.Sprintf("external: %d", i))
		data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
		if err != nil {
			t.Fatal(err)
		}
		_, lease, err := store.KeepExternallyOwned(t.Context(), data, digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
		pins = append(pins, digest)
	}
	restarted := Store{Root: store.Root, Blobs: blobs}
	writeFixture(t, source, "config/workflow.yaml", "overflow")
	data, digest, err := Capture(t.Context(), filepath.Join(source, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Keep(t.Context(), data, digest, nil); err == nil {
		t.Fatal("evicted external history generation after restart")
	}
	for _, pin := range pins {
		if _, err := restarted.Load(t.Context(), pin); err != nil {
			t.Fatalf("external history lost %s: %v", pin, err)
		}
		if exists, err := blobs.Has(t.Context(), pin); err != nil || !exists {
			t.Fatalf("external archive lost %s: %v", pin, err)
		}
	}
}
