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

func TestWriteExistingSnapshotPathsSkipsWindowsReservedNames(t *testing.T) {
	repository := t.TempDir()
	runSnapshotGit(t, repository, "init", "--initial-branch=main")
	runSnapshotGit(t, repository, "commit", "--allow-empty", "-m", "base")
	if err := os.MkdirAll(filepath.Join(repository, "test", "ci"), 0o700); err != nil {
		t.Fatal(err)
	}
	names := []string{"NUL", "PRN", "AUX", "COM1", "LPT9", filepath.Join("test", "ci", "NUL")}
	for _, name := range names {
		extended := `\\?\` + filepath.Join(repository, name)
		if err := os.WriteFile(extended, []byte("local CI residue"), 0o600); err != nil {
			t.Fatalf("create reserved path %q: %v", name, err)
		}
		t.Cleanup(func() {
			if err := os.Remove(extended); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove reserved path %q: %v", name, err)
			}
		})
	}
	selected := runSnapshotGit(t, repository, "ls-files", "--others", "--exclude-standard", "-z")
	if !strings.Contains(selected, "test/ci/NUL") {
		t.Fatalf("git did not select reserved worktree path: %q", selected)
	}

	var paths bytes.Buffer
	if err := writeExistingSnapshotPaths(repository, []byte(bytes.Join([][]byte{
		[]byte("NUL"),
		[]byte("PRN"),
		[]byte("AUX"),
		[]byte("COM1"),
		[]byte("LPT9"),
		[]byte(filepath.Join("test", "ci", "NUL")),
		nil,
	}, []byte{0})), &paths); err != nil {
		t.Fatalf("classify reserved paths: %v", err)
	}
	if paths.Len() != 0 {
		t.Fatalf("reserved paths selected for capture: %q", paths.Bytes())
	}
	if snapshot, err := CaptureSnapshot(context.Background(), repository, "reserved-path-run", storageTestRecord().CreatedAt); err != nil || snapshot == "" {
		t.Fatalf("capture with reserved paths = %q, %v", snapshot, err)
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
