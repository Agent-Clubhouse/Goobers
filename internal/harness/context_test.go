package harness

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/journal"
)

// TestMaterializeContextRefusesToTraverseAWorkspaceSymlink covers #2413 for
// the context-materialization site: the write lands in the stage's own
// worktree, which may hold repository-controlled content, and it runs in the
// harness's own process before the spawned subprocess is sandboxed. A
// symlink planted at .goobers (or at the context dir itself) must not be
// followed — the resolve fails and nothing is created outside the workspace.
func TestMaterializeContextRefusesToTraverseAWorkspaceSymlink(t *testing.T) {
	journalRoot := t.TempDir()
	ptr, err := apiv1.WriteArtifact(journalRoot, "artifacts/impl/diff.patch", []byte("upstream diff\n"), "text/x-patch")
	if err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}

	workspace := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(workspace, ".goobers")); err != nil {
		t.Fatal(err)
	}

	rec := &fakeRecorder{dir: journalRoot}
	exec, err := NewExecutor(&FakeAdapter{}, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	env := testEnvelope(workspace)
	env.ContextPointers = []apiv1.ContextPointer{{Name: "implement.artifact[0]", Artifact: &ptr}}
	if _, err := exec.materializeContext(env); err == nil {
		t.Fatal("expected materializeContext to refuse to traverse the symlinked .goobers directory")
	}
	if _, err := os.Lstat(filepath.Join(outsideDir, "context")); !os.IsNotExist(err) {
		t.Fatalf("context directory was created outside the workspace through the symlink: err=%v", err)
	}
}

// A resumed engine parent's child-result continuation whose receipt is
// absent fails with the named retirement error, never only a missing file;
// any other absent pointer fails as before (#8085 d).
func TestMaterializeContextNamesARetiredChildResult(t *testing.T) {
	rec := &fakeRecorder{dir: t.TempDir()}
	exec, err := NewExecutor(&FakeAdapter{}, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	digest := journal.Digest([]byte("a receipt blob GC collected"))
	env := testEnvelope(t.TempDir())
	env.ContextPointers = []apiv1.ContextPointer{childresult.ContinuationPointer(digest, 64)}
	if _, err := exec.materializeContext(env); !errors.Is(err, childresult.ErrResultRetired) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("materialize a retired child result = %v, want ErrResultRetired over the missing file", err)
	}
	other := childresult.ContinuationPointer(digest, 64)
	other.Name = "implement.artifact[0]"
	env.ContextPointers = []apiv1.ContextPointer{other}
	if _, err := exec.materializeContext(env); err == nil || errors.Is(err, childresult.ErrResultRetired) {
		t.Fatalf("materialize another absent pointer = %v, want the plain missing-artifact failure", err)
	}
}
