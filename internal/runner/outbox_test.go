package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", full, err)
	}
}

func TestCollectOutboxFilesSingleFile(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "report.json", `{"ok":true}`)

	files, err := collectOutboxFiles(root, []string{"report.json"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	if len(files) != 1 || files[0].RelPath != "report.json" || string(files[0].Data) != `{"ok":true}` {
		t.Fatalf("files = %+v, want one report.json entry", files)
	}
}

func TestCollectOutboxFilesDirectoryRecursion(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "reports/a.txt", "a")
	writeWorkspaceFile(t, root, "reports/nested/b.txt", "b")

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.RelPath] = string(f.Data)
	}
	want := map[string]string{"reports/a.txt": "a", "reports/nested/b.txt": "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("file %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestCollectOutboxFilesMissingPathIsSkipped(t *testing.T) {
	root := t.TempDir()

	files, err := collectOutboxFiles(root, []string{"does-not-exist.txt"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %+v, want none for a missing declared path", files)
	}
}

func TestCollectOutboxFilesRejectsLexicalEscape(t *testing.T) {
	root := t.TempDir()

	for _, rel := range []string{"../secret.txt", "/etc/passwd", "a/../../b"} {
		if _, err := collectOutboxFiles(root, []string{rel}); err == nil {
			t.Fatalf("collectOutboxFiles(%q) succeeded, want a path-escape error", rel)
		}
	}
}

func TestCollectOutboxFilesRejectsSymlinkEscapeForDeclaredFile(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("host secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	if _, err := collectOutboxFiles(root, []string{"escape-link"}); err == nil {
		t.Fatal("collectOutboxFiles(escape-link) succeeded, want a symlink-escape error")
	}
}

func TestCollectOutboxFilesSkipsSymlinkedSubdirectoryDuringWalk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeWorkspaceFile(t, outside, "leaked.txt", "host secret")
	writeWorkspaceFile(t, root, "reports/kept.txt", "kept")

	link := filepath.Join(root, "reports", "escape-dir")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f.RelPath, "leaked") {
			t.Fatalf("collected a file through a symlinked subdirectory: %+v", f)
		}
	}
	var sawKept bool
	for _, f := range files {
		if f.RelPath == "reports/kept.txt" {
			sawKept = true
		}
	}
	if !sawKept {
		t.Fatalf("expected reports/kept.txt among collected files, got %+v", files)
	}
}

func TestCollectOutboxFilesSkipsSymlinkedFileDuringWalk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("host secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	writeWorkspaceFile(t, root, "reports/kept.txt", "kept")
	link := filepath.Join(root, "reports", "linked.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	files, err := collectOutboxFiles(root, []string{"reports"})
	if err != nil {
		t.Fatalf("collectOutboxFiles: %v", err)
	}
	for _, f := range files {
		if f.RelPath == "reports/linked.txt" {
			t.Fatalf("collected a symlinked file during directory walk: %+v", f)
		}
	}
}

func TestCollectOutboxFilesEnforcesAggregateByteLimit(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", journal.MaxOutboxBytesPerAttempt/2+1)
	writeWorkspaceFile(t, root, "a.bin", big)
	writeWorkspaceFile(t, root, "b.bin", big)

	if _, err := collectOutboxFiles(root, []string{"a.bin", "b.bin"}); err == nil {
		t.Fatal("collectOutboxFiles over the aggregate byte limit succeeded, want an error")
	}
}

func TestCollectOutboxFilesEnforcesFileCountLimit(t *testing.T) {
	root := t.TempDir()
	declared := make([]string, 0, journal.MaxOutboxFilesPerAttempt+1)
	for i := 0; i < journal.MaxOutboxFilesPerAttempt+1; i++ {
		rel := filepath.ToSlash(filepath.Join("many", "f"+strconv.Itoa(i)+".txt"))
		writeWorkspaceFile(t, root, rel, "x")
		declared = append(declared, rel)
	}

	if _, err := collectOutboxFiles(root, declared); err == nil {
		t.Fatal("collectOutboxFiles over the file-count limit succeeded, want an error")
	}
}

// TestRunnerExportOutboxEndToEnd exercises the (*Runner).exportOutbox wrapper
// dispatchTask calls: workspace files in, journal-recorded artifacts out,
// through the real *journal.Run (satisfying executionJournal) rather than a
// mock, so the wiring between the collector and journal.ExportOutbox is
// covered, not just each half in isolation.
func TestRunnerExportOutboxEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "report.json", `{"ok":true}`)

	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-e2e"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}

	t.Cleanup(func() { _ = jr.Close() })

	var r *Runner
	task := apiv1.Task{Name: "build", Outbox: []string{"report.json"}}
	if err := r.exportOutbox(jr, root, task, 1, journal.AttemptPolicy); err != nil {
		t.Fatalf("exportOutbox: %v", err)
	}

	want := filepath.Join(runsDir, "run-outbox-e2e", "artifacts", "outbox", "build", "attempt-1", "report.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected exported outbox file at %s: %v", want, err)
	}
}

func TestRunnerExportOutboxMirrorsDurableCopy(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "reports/report.json", `{"token":"scrubbed"}`)
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-mirror"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	mirror := t.TempDir()
	task := apiv1.Task{
		Name:             "build",
		Outbox:           []string{"reports/report.json"},
		OutboxMirrorPath: mirror,
	}
	var r *Runner
	if err := r.exportOutbox(jr, root, task, 2, journal.AttemptPolicy); err != nil {
		t.Fatalf("exportOutbox: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(mirror, "run-outbox-mirror", "build", "attempt-2", "reports", "report.json"))
	if err != nil {
		t.Fatalf("read mirrored outbox: %v", err)
	}
	if string(got) != `{"token":"scrubbed"}` {
		t.Fatalf("mirrored content = %q", got)
	}
}

func TestMirrorOutboxRejectsRelativeRoot(t *testing.T) {
	if err := mirrorOutbox(t.TempDir(), "relative/path", nil); err == nil {
		t.Fatal("mirrorOutbox accepted a relative root")
	}
}

func TestMakeContainedDirRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if _, err := makeContainedDir(root, filepath.Join("escape", "nested")); err == nil {
		t.Fatal("makeContainedDir followed a symlink outside the mirror root")
	}
	if _, err := os.Stat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Fatalf("escape created an outside directory, stat err = %v", err)
	}
}

// TestRunnerExportOutboxPropagatesPathEscape confirms a declared path that
// escapes the workspace fails the exportOutbox call closed rather than
// silently skipping the offending entry — the class of gap the #1552 prior
// escalation flagged.
func TestRunnerExportOutboxPropagatesPathEscape(t *testing.T) {
	root := t.TempDir()
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-escape"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	var r *Runner
	task := apiv1.Task{Name: "build", Outbox: []string{"../../etc/passwd"}}
	if err := r.exportOutbox(jr, root, task, 1, journal.AttemptPolicy); err == nil {
		t.Fatal("exportOutbox with an escaping declared path succeeded, want an error")
	}
}

// TestRunnerExportOutboxNoOpWithoutDeclaration confirms a task that declares
// no Outbox paths never touches the journal.
func TestRunnerExportOutboxNoOpWithoutDeclaration(t *testing.T) {
	root := t.TempDir()
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: "run-outbox-noop"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	var r *Runner
	task := apiv1.Task{Name: "build"}
	if err := r.exportOutbox(jr, root, task, 1, journal.AttemptPolicy); err != nil {
		t.Fatalf("exportOutbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runsDir, "run-outbox-noop", "artifacts", "outbox")); !os.IsNotExist(err) {
		t.Fatalf("expected no outbox directory, stat err = %v", err)
	}
}
