//go:build unix

package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPinnedCustodyRefusesSubstitutedPaths(t *testing.T) {
	for _, component := range []string{"pin", pinnedCustodyFile} {
		t.Run(component, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := repoKey("repo")
			root := filepath.Join(m.pinnedRoot, key)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			if component == pinnedCustodyFile {
				if err := os.Mkdir(filepath.Join(root, "pin"), 0o700); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(target, "owner.json")
				if err := writeMarker(target, marker{RunID: "owner", OwnerRunID: "owner", RepositoryDigest: RepositoryDigest("repo"), CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(root, component)); err != nil {
				t.Fatal(err)
			}
			if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { t.Fatal("followed substituted path"); return nil }); err != nil {
				t.Fatal(err)
			}
			if err := m.handoffPinnedState(context.Background(), key, "owner"); err == nil {
				t.Fatal("accepted substituted path")
			}
		})
	}
}
