package recovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

func TestCaptureSnapshotClassifiesWindowsReservedNamesAndAllowsRepass(t *testing.T) {
	repository := t.TempDir()
	runSnapshotGit(t, repository, "init", "--initial-branch=main")
	runSnapshotGit(t, repository, "commit", "--allow-empty", "-m", "base")
	worktree := filepath.Join(t.TempDir(), "run")
	runSnapshotGit(t, repository, "worktree", "add", "--detach", worktree, "main")
	names := []string{"NUL", "PRN", "AUX", "COM1", "LPT9", filepath.Join("test", "ci", "NUL")}
	writeReservedSnapshotPaths(t, worktree, names)
	selected := runSnapshotGit(t, worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if !strings.Contains(selected, "test/ci/NUL") {
		t.Fatalf("git did not select reserved worktree path: %q", selected)
	}
	var paths bytes.Buffer
	excluded, err := writeExistingSnapshotPaths(worktree, []byte(bytes.Join([][]byte{
		[]byte("NUL"),
		[]byte("PRN"),
		[]byte("AUX"),
		[]byte("COM1"),
		[]byte("LPT9"),
		[]byte(filepath.Join("test", "ci", "NUL")),
		nil,
	}, []byte{0})), &paths)
	if err != nil {
		t.Fatalf("classify reserved paths: %v", err)
	}
	if paths.Len() != 0 {
		t.Fatalf("reserved paths selected for capture: %q", paths.Bytes())
	}
	if len(excluded) != len(names) {
		t.Fatalf("excluded paths = %q, want %q", excluded, names)
	}
	snapshot, err := CaptureSnapshot(context.Background(), worktree, "reserved-path-run", storageTestRecord().CreatedAt)
	if err != nil || snapshot == "" {
		t.Fatalf("capture with reserved paths = %q, %v", snapshot, err)
	}
	message := runSnapshotGit(t, worktree, "show", "-s", "--format=%B", snapshot)
	for _, name := range names {
		name = filepath.ToSlash(name)
		if !strings.Contains(message, `Recovery-Excluded-Path: "`+name+`"`) {
			t.Errorf("snapshot message does not classify %q: %q", name, message)
		}
	}

	removeReservedSnapshotPaths(t, worktree, names)
	runSnapshotGit(t, repository, "worktree", "remove", "--force", worktree)
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("cleanup left worktree at %q: %v", worktree, err)
	}
	runSnapshotGit(t, repository, "worktree", "add", "--detach", worktree, "main")
	writeReservedSnapshotPaths(t, worktree, []string{filepath.Join("test", "ci", "NUL")})
	if repass, err := CaptureSnapshot(context.Background(), worktree, "reserved-path-repass", storageTestRecord().CreatedAt); err != nil || repass == "" {
		t.Fatalf("repass capture with reserved path = %q, %v", repass, err)
	}
	removeReservedSnapshotPaths(t, worktree, []string{filepath.Join("test", "ci", "NUL")})
	runSnapshotGit(t, repository, "worktree", "remove", "--force", worktree)
}

func writeReservedSnapshotPaths(t *testing.T, repository string, names []string) {
	t.Helper()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repository, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		extended := `\\?\` + filepath.Join(repository, name)
		if err := os.WriteFile(extended, []byte("local CI residue"), 0o600); err != nil {
			t.Fatalf("create reserved path %q: %v", name, err)
		}
	}
}

func removeReservedSnapshotPaths(t *testing.T, repository string, names []string) {
	t.Helper()
	for _, name := range names {
		extended := `\\?\` + filepath.Join(repository, name)
		if err := os.Remove(extended); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove reserved path %q: %v", name, err)
		}
	}
}

func runSnapshotGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := testgit.Command(append([]string{"-C", repository}, args...)...)
	command.Env = append(command.Env,
		"GIT_AUTHOR_NAME=Recovery Test",
		"GIT_AUTHOR_EMAIL=recovery@example.invalid",
		"GIT_COMMITTER_NAME=Recovery Test",
		"GIT_COMMITTER_EMAIL=recovery@example.invalid",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
