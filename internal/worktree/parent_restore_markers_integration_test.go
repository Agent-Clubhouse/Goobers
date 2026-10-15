//go:build integration

package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentRestoreReclaimsInterruptedCleanup(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"pending", "interrupted-hold", "interrupted-release", "interrupted-cleanup", "missing-primary", "missing-ownership", "missing-start", "missing-ownership-start", "foreign-owner", "foreign-head", "foreign-start", "quarantined"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			url := newSourceRepo(t)
			wt, err := m.Create(t.Context(), CreateOptions{RepoURL: url, RunID: "parent-stage", OwnerRunID: "parent", Gaggle: "gaggle", BaseRef: "main", Branch: "goobers/parent", RetainOnCleanup: true})
			if err != nil {
				t.Fatal(err)
			}
			custody, err := wt.HoldForChild(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD"))
			mustWriteFile(t, filepath.Join(wt.Path, "staged.txt"), "staged\n")
			runTestGit(t, wt.Path, "add", "staged.txt")
			mustWriteFile(t, filepath.Join(wt.Path, "staged.txt"), "dirty\n")
			primary, ownership, err := wt.custodyMarkers()
			if err != nil {
				t.Fatal(err)
			}
			wantFailure := false
			switch mode {
			case "pending":
				primary.Status, primary.CleanupDisposition = statusCleanupPending, ""
				ownership = primary
			case "interrupted-hold":
				primary.Status, primary.CleanupDisposition = statusActive, ""
			case "interrupted-release":
				ownership.Status, ownership.CleanupDisposition = statusActive, ""
			case "interrupted-cleanup":
				primary.Status, primary.CleanupDisposition = statusActive, ""
				ownership.Status, ownership.CleanupDisposition = statusCleanupPending, ""
			case "foreign-owner":
				ownership.OwnerRunID, wantFailure = "foreign", true
			case "foreign-start":
				ownership.StartRef, wantFailure = strings.Repeat("a", 40), true
			case "missing-start":
				primary.StartRef, ownership.StartRef = "", ""
			case "missing-ownership-start":
				ownership.StartRef = ""
			case "foreign-head":
				runTestGit(t, wt.Path, "commit", "--allow-empty", "-m", "unexpected advance")
				wantFailure = true
			case "quarantined":
				primary.CleanupDisposition, wantFailure = CleanupDispositionRetryExhausted, true
				ownership = primary
			}
			if err := wt.writeCustodyMarkers(primary, ownership); err != nil {
				t.Fatal(err)
			}
			if mode == "missing-primary" {
				if err := os.Remove(m.markerPath(wt.key, wt.RunID)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-ownership" {
				if err := os.Remove(m.ownershipPath(wt.key, primary.Directory)); err != nil {
					t.Fatal(err)
				}
			}
			opts := ParentRestoreOptions{RepoURL: url, RunID: wt.RunID, OwnerRunID: "parent", Gaggle: "gaggle", Branch: wt.Branch, BaseRef: primary.BaseRef, HeadSHA: head, StartRef: custody.StartRef}
			_, err = m.CreateParentRestore(t.Context(), opts)
			if (err != nil) != wantFailure {
				t.Fatal("interrupted cleanup restoration", err)
			}
			if got := strings.TrimSpace(runTestGit(t, wt.Path, "show", ":staged.txt")); got != "staged" {
				t.Fatal("restoration reset staged content", got)
			}
			if got, err := os.ReadFile(filepath.Join(wt.Path, "staged.txt")); err != nil || string(got) != "dirty\n" {
				t.Fatal("restoration reset working content", string(got), err)
			}
			if wantFailure {
				got, err := readMarker(m.markerPath(wt.key, wt.RunID))
				if err != nil || got.Status != primary.Status || got.CleanupDisposition != primary.CleanupDisposition {
					t.Fatal("refused restoration changed custody", err)
				}
				return
			}
			if got, err := m.AdoptHeldStage(t.Context(), url, custody); err != nil || got.Path != wt.Path {
				t.Fatal("restoration failed to reestablish exact hold", err)
			}
		})
	}
}

func TestIntegrationParentRestoreRecreatesAbsentOwnedCheckout(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"missing-checkout", "empty-checkout", "missing-primary", "provisional", "foreign-owner", "foreign-empty", "foreign-registration", "unowned-registration"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			url := newSourceRepo(t)
			wt, err := m.Create(t.Context(), CreateOptions{RepoURL: url, RunID: "parent-stage", OwnerRunID: "parent", Gaggle: "gaggle", BaseRef: "main", Branch: "goobers/parent", RetainOnCleanup: true})
			if err != nil {
				t.Fatal(err)
			}
			custody, err := wt.HoldForChild(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "HEAD"))
			primary, ownership, err := wt.custodyMarkers()
			if err != nil {
				t.Fatal(err)
			}
			primaryPath, ownershipPath := m.markerPath(wt.key, wt.RunID), m.ownershipPath(wt.key, primary.Directory)
			if mode == "foreign-registration" {
				runTestGit(t, wt.Path, "switch", "-c", "goobers/foreign")
			}
			if mode == "provisional" {
				runTestGit(t, m.repoDirForKey(wt.key), "worktree", "remove", "--force", wt.Path)
				primary.StartRef, ownership.StartRef = "", ""
				primary.Status, ownership.Status = statusActive, statusActive
				primary.CleanupDisposition, ownership.CleanupDisposition = "", ""
			} else if err := os.RemoveAll(wt.Path); err != nil {
				t.Fatal(err)
			}
			if mode == "empty-checkout" || mode == "foreign-empty" || mode == "foreign-registration" {
				if err := os.Mkdir(wt.Path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "foreign-owner" || mode == "foreign-empty" {
				ownership.OwnerRunID = "another-parent"
			}
			if err := wt.writeCustodyMarkers(primary, ownership); err != nil {
				t.Fatal(err)
			}
			if mode == "missing-primary" || mode == "unowned-registration" {
				if err := os.Remove(primaryPath); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unowned-registration" {
				if err := os.Remove(ownershipPath); err != nil {
					t.Fatal(err)
				}
			}
			opts := ParentRestoreOptions{RepoURL: url, RunID: wt.RunID, OwnerRunID: "parent", Gaggle: "gaggle", Branch: wt.Branch, BaseRef: primary.BaseRef, HeadSHA: head, StartRef: custody.StartRef}
			restored, err := m.CreateParentRestore(t.Context(), opts)
			if mode == "foreign-owner" || mode == "foreign-empty" || mode == "foreign-registration" || mode == "unowned-registration" {
				if err == nil {
					t.Fatal("borrowed unowned checkout registration")
				}
				if registered, err := worktreeRegistered(t.Context(), m.repoDirForKey(wt.key), wt.Path); err != nil || !registered {
					t.Fatal("refused restoration removed registration", registered, err)
				}
				if mode == "foreign-empty" || mode == "foreign-registration" {
					if _, err := os.Lstat(wt.Path); err != nil {
						t.Fatal("refused restoration removed foreign directory", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal("restore absent owned checkout", err)
			}
			if got, err := restored.HoldForChild(t.Context()); err != nil || got != custody {
				t.Fatal("restoration lost original custody", got, err)
			}
			if got := strings.TrimSpace(runTestGit(t, restored.Path, "rev-parse", "HEAD")); got != head {
				t.Fatal("restoration changed original branch", got)
			}
		})
	}
}
