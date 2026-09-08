//go:build integration

package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCaptureDoesNotAcknowledgeUnarchivedNestedState(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "parent")
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	nested := filepath.Join(repository, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, nested, "init", "--initial-branch=main")
	recoveryTestGit(t, nested, "commit", "--allow-empty", "-m", "nested")
	path := filepath.Join(nested, "untracked.bin")
	if err := os.WriteFile(path, []byte{0, 255, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := CaptureSnapshot(context.Background(), repository, "nested-run", storageTestRecord().CreatedAt); !errors.Is(err, ErrNestedRecoveryRequired) || snapshot != "" {
		t.Fatalf("parent-only snapshot acknowledged nested recovery: %q %v", snapshot, err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
		t.Fatalf("failed capture changed parent HEAD: %s", got)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "\x00\xff\x01" {
		t.Fatalf("nested work was lost: %q %v", got, err)
	}
}
