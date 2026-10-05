package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestartRequiresReleasedSourceWorkspace(t *testing.T) {
	for _, state := range []status{statusActive, statusCleanupPending, statusCleanupRetained, statusKept} {
		t.Run(string(state), func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := m.AssertRunReleased(t.Context(), "source"); err != nil {
				t.Fatal(err)
			}
			mk := marker{RunID: "source-implement", OwnerRunID: "source", Status: state}
			if err := writeMarker(m.markerPath("repo", mk.RunID), mk); err != nil {
				t.Fatal(err)
			}
			if err := m.AssertRunReleased(t.Context(), "source"); err == nil {
				t.Fatal("held source silently omitted")
			}
			if err := m.AssertRunReleased(t.Context(), "another"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(m.markerPath("repo", mk.RunID)); err != nil {
				t.Fatal("inspection changed custody", err)
			}
		})
	}
}

func TestRestartRefusesUnknownCustodyAndOrphanCheckouts(t *testing.T) {
	for _, mode := range []string{"unreadable", "orphan", "ownership-only"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unreadable":
				if err := os.MkdirAll(m.markersDirForKey("repo"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(m.markerPath("repo", "unknown"), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "orphan":
				if err := os.MkdirAll(filepath.Join(m.runsDirForKey("repo"), "wt-unknown"), 0700); err != nil {
					t.Fatal(err)
				}
			case "ownership-only":
				if err := writeMarker(m.ownershipPath("repo", "held"), marker{RunID: "source-task", OwnerRunID: "source", Status: statusKept}); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.AssertRunReleased(t.Context(), "source"); err == nil {
				t.Fatal("uncertain source state accepted")
			}
		})
	}
}
