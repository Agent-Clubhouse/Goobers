package diagnostictmp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testNamespacePath(t *testing.T, root string) string {
	t.Helper()
	name, err := namespaceDirectoryName()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, name)
}

func TestNamespaceSeparatesUsersSharingTempRoot(t *testing.T) {
	first := namespaceForUserID("1001")
	if first != namespaceForUserID("1001") || first == namespaceForUserID("1002") {
		t.Fatal("namespace is not stable and scoped to one user")
	}
	for _, identity := range []string{"1001", "1002", "S-1-5-21-synthetic-owner"} {
		name := namespaceForUserID(identity)
		if !strings.HasPrefix(name, namespaceName+"-") || filepath.Base(name) != name {
			t.Fatal("owner identity did not produce a contained namespace name")
		}
	}
}

func TestSweepPreservesForeignActiveScopesAndRemovesAbandonedScopes(t *testing.T) {
	root := t.TempDir()
	active, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.Reclaim() })
	var abandoned []*Scope
	for range 3 {
		scope, err := Establish(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scope.Dir(), "debug.log"), []byte("synthetic raw diagnostics"), 0o600); err != nil {
			t.Fatal(err)
		}
		abandoned = append(abandoned, scope)
		// Simulate a crashed owner releasing its OS handle without reclaiming.
		// Establish runs a safe sweep, so postpone release until all exist.
		t.Cleanup(func() { _ = scope.Reclaim() })
	}
	// A separate handle in the same process has the same exclusive-lock
	// conflict as a separate daemon; no shared in-memory registry is used.
	removed, err := Sweep(root)
	if err != nil || removed != 0 {
		t.Fatalf("live scopes reclaimed: removed=%d error=%v", removed, err)
	}
	for _, scope := range abandoned {
		if err := scope.lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	removed, err = Sweep(root)
	if err != nil || removed != len(abandoned) {
		t.Fatalf("abandoned scopes retained: removed=%d error=%v", removed, err)
	}
	for _, scope := range abandoned {
		if _, err := os.Stat(scope.Dir()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("abandoned raw diagnostic directory survived sweep")
		}
	}
	if _, err := os.Stat(active.Dir()); err != nil {
		t.Fatal("foreign active scope was removed")
	}
}

func TestEstablishCollectsAbandonedScopesAndKeepsActiveForeignScope(t *testing.T) {
	root := t.TempDir()
	active, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.Reclaim() })
	var abandoned []*Scope
	for range 3 {
		scope, err := Establish(root)
		if err != nil {
			t.Fatal(err)
		}
		abandoned = append(abandoned, scope)
	}
	for _, scope := range abandoned {
		if err := scope.lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(testNamespacePath(t, root), "unrelated")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	next, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Reclaim() })
	for _, scope := range abandoned {
		if _, err := os.Stat(scope.Dir()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("abandoned scope survived per-capture sweep")
		}
		if _, err := os.Stat(scope.Dir() + leaseSuffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("abandoned lease survived per-capture sweep")
		}
	}
	for _, path := range []string{active.Dir(), next.Dir(), unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active or unrelated directory removed: %v", err)
		}
	}
}

func TestReclaimIsPrivateIdempotentAndLeavesOnlyNamespaceLock(t *testing.T) {
	root := t.TempDir()
	scope, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(scope.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatal("diagnostics scope is not private")
	}
	if err := scope.Reclaim(); err != nil {
		t.Fatal(err)
	}
	if err := scope.Reclaim(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(testNamespacePath(t, root))
	if err != nil || len(entries) != 1 || entries[0].Name() != namespaceLockName {
		t.Fatalf("reclaim retained scope files: count=%d error=%v", len(entries), err)
	}
}

func TestNamespaceAndLockSymlinksFailClosed(t *testing.T) {
	name := filepath.Base(testNamespacePath(t, ""))
	for _, target := range []string{name, filepath.Join(name, namespaceLockName)} {
		t.Run(target, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			path := filepath.Join(root, target)
			if target != name {
				if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				outside = filepath.Join(outside, "sentinel")
				if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if _, err := Establish(root); err == nil {
				t.Fatal("followed a diagnostics namespace or lock symlink")
			}
			if _, err := Sweep(root); err == nil {
				t.Fatal("swept through a diagnostics namespace or lock symlink")
			}
		})
	}
}

func TestNamespaceLockWaitIsBounded(t *testing.T) {
	root := t.TempDir()
	_, guard, err := acquireNamespace(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Release() }()
	started := time.Now()
	_, _, err = acquireNamespace(root, 20*time.Millisecond)
	if !errors.Is(err, errNamespaceBusy) || time.Since(started) > time.Second {
		t.Fatalf("namespace contention was not bounded: %v", err)
	}
}

func TestConcurrentCapturesAndSweepsPreserveEveryLiveScope(t *testing.T) {
	root := t.TempDir()
	var workers sync.WaitGroup
	for range 6 {
		workers.Go(func() {
			for range 8 {
				scope, err := Establish(root)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := Sweep(root); err != nil {
					t.Error(err)
				}
				if _, err := os.Stat(scope.Dir()); err != nil {
					t.Error("concurrent sweep removed an active capture")
				}
				if err := scope.Reclaim(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Wait()
	entries, err := os.ReadDir(testNamespacePath(t, root))
	if err != nil || len(entries) != 1 || entries[0].Name() != namespaceLockName {
		t.Fatalf("concurrent captures retained scopes: count=%d error=%v", len(entries), err)
	}
}
