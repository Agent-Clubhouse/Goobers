package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRootIdentityInspectionNeverAdoptsLegacyRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadRootIdentity(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing identity: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("inspection wrote state: %v %v", entries, err)
	}
	if _, err := EnsureRootIdentity(context.Background(), root); err == nil {
		t.Fatal("adopted unprovisioned directory")
	}
}

func TestRootIdentityInitStableAndDistinct(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if _, err := Init(first); err != nil {
		t.Fatal(err)
	}
	id, err := ReadRootIdentity(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(first); err != nil {
		t.Fatal(err)
	}
	again, err := ReadRootIdentity(first)
	if err != nil || again != id {
		t.Fatalf("init changed identity: %q -> %q, %v", id, again, err)
	}
	if _, err := Init(second); err != nil {
		t.Fatal(err)
	}
	other, err := ReadRootIdentity(second)
	if err != nil || other == id {
		t.Fatalf("roots share identity: %q %q %v", id, other, err)
	}
}

func TestRootIdentityConcurrentLegacyAdoption(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(NewLayout(root).ConfigFile(), []byte("existing config"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const count = 16
	ids, errs := make([]string, count), make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() { ids[i], errs[i] = EnsureRootIdentity(ctx, root) })
	}
	wg.Wait()
	for i := range count {
		if errs[i] != nil || ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("split identity: ids=%v errors=%v", ids, errs)
		}
	}
}

func TestRootIdentityCorruptionIsNotReplaced(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, RootIdentityFileName)
	if err := os.WriteFile(path, []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRootIdentity(context.Background(), root); err == nil {
		t.Fatal("corruption silently re-keyed")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "broken\n" {
		t.Fatalf("corrupt identity overwritten: %q %v", data, err)
	}
}
