//go:build integration

package recovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// A Git-clean checkout whose worktree bytes legitimately differ from the
// canonical blob (line endings, working-tree encoding, ident expansion) must
// not record those paths as edits (#6917), while genuine edits, filtered
// content and untracked data are still retained as raw bytes.
func TestIntegrationCaptureSnapshotKeepsCleanCheckoutConversions(t *testing.T) {
	testdep.Require(t, "git")
	for _, tc := range []struct {
		name       string
		attributes string
		autocrlf   string
	}{
		{"eol=crlf autocrlf=false", "* text=auto eol=lf\n*.ps1 text eol=crlf\n", "false"},
		{"eol=crlf autocrlf=true", "* text=auto eol=lf\n*.ps1 text eol=crlf\n", "true"},
		{"eol=lf autocrlf=false", "* text=auto eol=lf\n*.ps1 text eol=lf\n", "false"},
		{"text=auto autocrlf=true", "", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := t.TempDir()
			recoveryTestGit(t, repository, "init", "--initial-branch=main")
			recoveryTestGit(t, repository, "config", "core.autocrlf", tc.autocrlf)
			recoveryTestGit(t, repository, "config", "core.safecrlf", "false")
			checkout := "\r\n"
			if tc.attributes != "" {
				writeRestoreFixture(t, repository, ".gitattributes", tc.attributes)
				if tc.name == "eol=lf autocrlf=false" {
					checkout = "\n"
				}
			}
			for _, name := range []string{"Unchanged.ps1", "Edited.ps1", "Removed.ps1"} {
				writeRestoreFixture(t, repository, name, "Write-Output 1"+checkout+"Write-Output 2"+checkout)
			}
			recoveryTestGit(t, repository, "add", ".")
			recoveryTestGit(t, repository, "commit", "-m", "base")
			head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
			if blob := recoveryTestGit(t, repository, "cat-file", "blob", "HEAD:Unchanged.ps1"); bytes.Contains([]byte(blob), []byte("\r")) {
				t.Fatalf("fixture blob is not canonical LF: %q", blob)
			}
			if status := recoveryTestGit(t, repository, "status", "--porcelain"); status != "" {
				t.Fatalf("fixture checkout is not clean: %q", status)
			}
			// A stat-dirty but content-clean file must be compared by content.
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(repository, "Unchanged.ps1"), future, future); err != nil {
				t.Fatal(err)
			}
			writeRestoreFixture(t, repository, "Edited.ps1", "Write-Output 3"+checkout)
			if err := os.Remove(filepath.Join(repository, "Removed.ps1")); err != nil {
				t.Fatal(err)
			}
			writeRestoreFixture(t, repository, "Untracked.ps1", "Write-Output 4\r\n")
			indexPath := filepath.Join(repository, ".git", "index")
			before, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := CaptureSnapshot(context.Background(), repository, "run-crlf", storageTestRecord().CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if got := recoveryTestGit(t, repository, "diff", "--name-status", "--no-renames", head, snapshot); got != "M\tEdited.ps1\nD\tRemoved.ps1\nA\tUntracked.ps1" {
				t.Fatalf("snapshot changes = %q, want only the genuine edit, deletion and untracked file", got)
			}
			for name, want := range map[string]string{"Edited.ps1": "Write-Output 3" + checkout, "Untracked.ps1": "Write-Output 4\r\n"} {
				var captured bytes.Buffer
				if err := recoveryGit(context.Background(), repository, &captured, "cat-file", "blob", snapshot+":"+name); err != nil || captured.String() != want {
					t.Fatalf("%s raw bytes = %q, want %q: %v", name, captured.String(), want, err)
				}
			}
			if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
				t.Fatalf("capture changed HEAD: %s", got)
			}
			after, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("capture changed caller's index: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(repository, "Unchanged.ps1")); err != nil || string(got) != "Write-Output 1"+checkout+"Write-Output 2"+checkout {
				t.Fatalf("capture changed working file: %q %v", got, err)
			}
		})
	}
}

func TestIntegrationCaptureSnapshotRetainsFilteredAndHiddenTrackedBytes(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	writeRestoreFixture(t, repository, ".gitattributes", "*.lfs filter=pointer\n*.utf16 text working-tree-encoding=UTF-16LE eol=lf\n")
	writeRestoreFixture(t, repository, "hidden.txt", "base\n")
	writeRestoreFixture(t, repository, "encoded.utf16", "h\x00i\x00\n\x00")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	// Store a pointer blob as LFS does, while the checkout holds real content.
	recoveryTestGit(t, repository, "config", "filter.pointer.clean", "git hash-object --stdin")
	recoveryTestGit(t, repository, "config", "filter.pointer.required", "true")
	writeRestoreFixture(t, repository, "large.lfs", "actual content")
	recoveryTestGit(t, repository, "add", "large.lfs")
	recoveryTestGit(t, repository, "commit", "-m", "pointer")
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	// Capture must neither run the filter nor fail because it is unavailable.
	recoveryTestGit(t, repository, "config", "filter.pointer.clean", "false")
	writeRestoreFixture(t, repository, "hidden.txt", "edited\n")
	recoveryTestGit(t, repository, "update-index", "--assume-unchanged", "hidden.txt")
	snapshot, err := CaptureSnapshot(context.Background(), repository, "run-filtered", storageTestRecord().CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repository, "diff", "--name-only", head, snapshot); got != "hidden.txt\nlarge.lfs" {
		t.Fatalf("snapshot changes = %q, want the assume-unchanged edit and filtered content", got)
	}
	for name, want := range map[string]string{"hidden.txt": "edited\n", "large.lfs": "actual content"} {
		var captured bytes.Buffer
		if err := recoveryGit(context.Background(), repository, &captured, "cat-file", "blob", snapshot+":"+name); err != nil || captured.String() != want {
			t.Fatalf("%s = %q, want raw %q: %v", name, captured.String(), want, err)
		}
	}
}

// Restore must reproduce the chosen representation end to end: an unchanged
// CRLF checkout keeps main's canonical blob instead of relying on a cached
// three-way apply to normalize a manufactured CRLF delta away.
func TestIntegrationRestoreSnapshotKeepsCleanCRLFCheckoutCanonical(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "config", "core.autocrlf", "false")
	writeRestoreFixture(t, repository, ".gitattributes", "* text=auto eol=lf\n*.ps1 text eol=crlf\n")
	writeRestoreFixture(t, repository, "Unchanged.ps1", "Write-Output 1\r\n")
	writeRestoreFixture(t, repository, "Edited.ps1", "Write-Output 2\r\n")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	unchanged := recoveryTestGit(t, repository, "rev-parse", "HEAD:Unchanged.ps1")
	recoveryTestGit(t, repository, "checkout", "-b", "implementation")
	writeRestoreFixture(t, repository, "Edited.ps1", "Write-Output 3\r\n")
	var err error
	record.SnapshotSHA, err = CaptureSnapshot(context.Background(), repository, record.RunID, record.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	recoveryTestGit(t, repository, "checkout", "--force", "main")
	writeRestoreFixture(t, repository, "Main.ps1", "Write-Output 4\r\n")
	recoveryTestGit(t, repository, "add", "Main.ps1")
	recoveryTestGit(t, repository, "commit", "-m", "advance main")
	main := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	restored, err := RestoreSnapshot(context.Background(), repository, record, main, "recovered", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repository, "diff", "--name-only", main, restored); got != "Edited.ps1" {
		t.Fatalf("restored changes = %q, want only the genuine edit", got)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", restored+":Unchanged.ps1"); got != unchanged {
		t.Fatalf("restored unchanged blob = %s, want canonical %s", got, unchanged)
	}
	var edited bytes.Buffer
	if err := recoveryGit(context.Background(), repository, &edited, "cat-file", "blob", restored+":Edited.ps1"); err != nil || edited.String() != "Write-Output 3\r\n" {
		t.Fatalf("restored edit = %q, want retained raw bytes: %v", edited.String(), err)
	}
}
