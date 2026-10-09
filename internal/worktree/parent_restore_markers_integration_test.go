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
	for _, mode := range []string{"pending", "interrupted-hold", "interrupted-release", "interrupted-cleanup", "foreign-owner", "foreign-head", "quarantined"} {
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
