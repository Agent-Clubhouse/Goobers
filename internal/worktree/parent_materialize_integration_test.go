//go:build integration

package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentRestoreCompletesOnlyUnfinishedMaterialization(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"files", "index", "both", "partial-clear", "dirty", "staged", "untracked", "foreign-intent", "no-intent"} {
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
			primary.ParentRestoreHead, ownership.ParentRestoreHead = head, head
			wantFailure := false
			switch mode {
			case "dirty":
				mustWriteFile(t, filepath.Join(wt.Path, "README.md"), "intervening edit\n")
				wantFailure = true
			case "staged":
				mustWriteFile(t, filepath.Join(wt.Path, "README.md"), "staged edit\n")
				runTestGit(t, wt.Path, "add", "README.md")
				wantFailure = true
			case "untracked":
				mustWriteFile(t, filepath.Join(wt.Path, "new.txt"), "untracked edit\n")
				wantFailure = true
			case "foreign-intent":
				ownership.ParentRestoreHead, wantFailure = strings.Repeat("a", 40), true
			case "partial-clear":
				ownership.ParentRestoreHead = ""
			case "no-intent":
				primary.ParentRestoreHead, ownership.ParentRestoreHead = "", ""
			}
			if mode == "files" || mode == "both" || mode == "no-intent" {
				if err := os.Remove(filepath.Join(wt.Path, "README.md")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "index" || mode == "both" {
				runTestGit(t, wt.Path, "read-tree", "--empty")
			}
			if err := wt.writeCustodyMarkers(primary, ownership); err != nil {
				t.Fatal(err)
			}
			before := runTestGit(t, wt.Path, "status", "--porcelain=v1", "-z")
			beforeIndex := runTestGit(t, wt.Path, "write-tree")
			beforeFile, _ := os.ReadFile(filepath.Join(wt.Path, "README.md"))
			opts := ParentRestoreOptions{RepoURL: url, RunID: wt.RunID, OwnerRunID: "parent", Gaggle: "gaggle", Branch: wt.Branch, BaseRef: primary.BaseRef, HeadSHA: head, StartRef: custody.StartRef}
			_, err = m.CreateParentRestore(t.Context(), opts)
			if (err != nil) != wantFailure {
				t.Fatal("materialization result", err)
			}
			after := runTestGit(t, wt.Path, "status", "--porcelain=v1", "-z")
			if wantFailure || mode == "no-intent" {
				afterFile, _ := os.ReadFile(filepath.Join(wt.Path, "README.md"))
				if after != before || runTestGit(t, wt.Path, "write-tree") != beforeIndex || string(afterFile) != string(beforeFile) {
					t.Fatal("restoration changed unowned or unexpected edits", before, after)
				}
				return
			}
			if after != "" {
				t.Fatal("checkout did not reach clean archive HEAD", after)
			}
			p, o, err := wt.custodyMarkers()
			if err != nil || p.ParentRestoreHead != "" || o.ParentRestoreHead != "" {
				t.Fatal("completed materialization retained authority", err)
			}
			mustWriteFile(t, filepath.Join(wt.Path, "README.md"), "later ordinary edit\n")
			if _, err := m.CreateParentRestore(t.Context(), opts); err != nil {
				t.Fatal("completed retry", err)
			}
			if got, err := os.ReadFile(filepath.Join(wt.Path, "README.md")); err != nil || string(got) != "later ordinary edit\n" {
				t.Fatal("completed retry reset later edits", string(got), err)
			}
		})
	}
}

func TestIntegrationParentRestoreCreationClearsMaterializationAuthority(t *testing.T) {
	testdep.Require(t, "git")
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	url := newSourceRepo(t)
	repository, err := m.WorkingCopy(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "main"))
	wt, err := m.CreateParentRestore(t.Context(), ParentRestoreOptions{RepoURL: url, RunID: "parent-stage", OwnerRunID: "parent", Gaggle: "gaggle", Branch: "goobers/parent", BaseRef: head, HeadSHA: head, StartRef: head})
	if err != nil {
		t.Fatal(err)
	}
	primary, ownership, err := wt.custodyMarkers()
	if err != nil || primary.ParentRestoreHead != "" || ownership.ParentRestoreHead != "" || primary.StartRef != head || ownership.StartRef != head {
		t.Fatal("creation returned before settling materialization authority", primary, ownership, err)
	}
}
