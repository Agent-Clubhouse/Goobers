package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

func TestInstanceIdentityDurableAndRootScoped(t *testing.T) {
	layout := NewLayout(t.TempDir())
	first, err := layout.EnsureIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("identity = %q, want 128-bit hexadecimal identifier", first)
	}
	for _, reader := range []Layout{NewLayout(layout.Root), layout.ForGaggle("other")} {
		got, err := reader.EnsureIdentity(context.Background())
		if err != nil || got != first {
			t.Fatalf("reopened identity = %q, %v; want %q", got, err, first)
		}
	}
	other, err := NewLayout(t.TempDir()).EnsureIdentity(context.Background())
	if err != nil || other == first {
		t.Fatalf("independent root identity = %q, %v", other, err)
	}
}

func TestInstanceIdentityReadDoesNotCreateState(t *testing.T) {
	layout := NewLayout(t.TempDir())
	if _, err := layout.ReadIdentity(); !os.IsNotExist(err) {
		t.Fatalf("missing identity error = %v", err)
	}
	entries, err := os.ReadDir(layout.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read created state: %v, %v", entries, err)
	}
}

func TestInstanceIdentityRejectsCorruptionWithoutRotation(t *testing.T) {
	for _, value := range []string{"", "not-an-id\n", strings.Repeat("0", 32) + "\n", strings.Repeat("a", 33), strings.Repeat("a", 8192)} {
		t.Run(value[:min(len(value), 12)], func(t *testing.T) {
			layout := NewLayout(t.TempDir())
			path := filepath.Join(layout.Root, "instance-id")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := layout.EnsureIdentity(context.Background()); err == nil {
				t.Fatal("corrupt identity was accepted or silently replaced")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != value {
				t.Fatalf("corrupt identity changed: %q, %v", got, err)
			}
		})
	}
}

func TestInstanceIdentityCanceledCreationWritesNothing(t *testing.T) {
	layout := NewLayout(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := layout.EnsureIdentity(ctx); err == nil {
		t.Fatal("canceled creation succeeded")
	}
	entries, err := os.ReadDir(layout.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled creation wrote state: %v, %v", entries, err)
	}
}

func TestInstanceIdentityConcurrentInitializersAgree(t *testing.T) {
	layout := NewLayout(t.TempDir())
	var group sync.WaitGroup
	values := make(chan string, 16)
	for range 16 {
		group.Go(func() {
			value, err := layout.EnsureIdentity(context.Background())
			if err != nil {
				t.Errorf("concurrent initializer: %v", err)
				return
			}
			values <- value
		})
	}
	group.Wait()
	close(values)
	stored, err := layout.ReadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for value := range values {
		if value != stored {
			t.Errorf("initializer returned %q, durable identity is %q", value, stored)
		}
	}
}

func TestInstanceIdentityReclaimsCrashFileAndSurvivesRename(t *testing.T) {
	parent := t.TempDir()
	layout := NewLayout(filepath.Join(parent, "before"))
	if err := os.Mkdir(layout.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Root, ".instance-id.pending"), []byte("interrupted write"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := layout.EnsureIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(layout.Root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("want exactly identity and lock, got %v, %v", entries, err)
	}
	renamed := filepath.Join(parent, "after")
	if err := os.Rename(layout.Root, renamed); err != nil {
		t.Fatal(err)
	}
	got, err := NewLayout(renamed).EnsureIdentity(context.Background())
	if err != nil || got != id {
		t.Fatalf("renamed root identity = %q, %v; want %q", got, err, id)
	}
}

func TestInstanceIdentityLockWaitHonorsCancellation(t *testing.T) {
	layout := NewLayout(t.TempDir())
	held, err := lock.TryAcquire(filepath.Join(layout.Root, ".instance-id.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := layout.EnsureIdentity(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended identity error = %v, want caller deadline", err)
	}
	if _, err := layout.ReadIdentity(); !os.IsNotExist(err) {
		t.Fatalf("contended initialization created identity: %v", err)
	}
}
