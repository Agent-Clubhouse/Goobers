package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootDiscoveryFindsLegacyAndHistoricalWithoutAdopting(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"old", "new"} {
		root := filepath.Join(base, name)
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(NewLayout(root).ConfigFile(), []byte("legacy fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DiscoverRoots(context.Background(), []string{base, base}, 100, 8)
	if err != nil || got.Partial || len(got.Paths) != 2 {
		t.Fatalf("discovery: %+v %v", got, err)
	}
	for _, root := range got.Paths {
		if _, err := ReadRootIdentity(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspection adopted root: %v", err)
		}
	}
}

func TestRootDiscoveryBoundsAndCancellation(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverRoots(context.Background(), []string{base}, 1, 0)
	if err != nil || !got.Partial || got.Visited != 1 {
		t.Fatalf("bound not visible: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverRoots(ctx, []string{base}, 100, 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestRootDiscoveryExplicitBaseGetsItsOwnDepthBudget(t *testing.T) {
	base := t.TempDir()
	child := filepath.Join(base, "child")
	root := filepath.Join(child, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(NewLayout(root).ConfigFile(), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverRoots(context.Background(), []string{base, child, root}, 100, 1)
	if err != nil || len(got.Paths) != 1 {
		t.Fatalf("explicit base skipped or root duplicated: %+v %v", got, err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || got.Paths[0] != canonical {
		t.Fatalf("root: %+v %v", got, err)
	}
}

func TestRootDiscoveryDoesNotFollowChildSymlinks(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(NewLayout(outside).ConfigFile(), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "linked-root")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := DiscoverRoots(context.Background(), []string{base}, 100, 8)
	if err != nil || len(got.Paths) != 0 {
		t.Fatalf("followed child symlink: %+v %v", got, err)
	}
	got, err = DiscoverRoots(context.Background(), []string{filepath.Join(base, "linked-root")}, 100, 8)
	if err != nil || len(got.Paths) != 1 {
		t.Fatalf("explicit symlink base not resolved: %+v %v", got, err)
	}
}
