//go:build integration

package recovery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationCaptureErrorMissingObject is #5352's first required
// regression: a recovery git operation against a ref/object that does not
// exist must name the git subcommand and carry the CaptureErrorMissingObject
// class, not a bare "exit status 128".
func TestIntegrationCaptureErrorMissingObject(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")

	err := recoveryGit(context.Background(), repository, io.Discard,
		"rev-parse", "--verify", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef^{commit}")
	if err == nil {
		t.Fatal("expected a failure resolving a nonexistent object")
	}
	var capture *CaptureError
	if !errors.As(err, &capture) {
		t.Fatalf("error %v is not a *CaptureError", err)
	}
	if capture.Subcommand != "rev-parse" {
		t.Errorf("Subcommand = %q, want %q", capture.Subcommand, "rev-parse")
	}
	if capture.ExitCode != 128 {
		t.Errorf("ExitCode = %d, want 128", capture.ExitCode)
	}
	if capture.Class != CaptureErrorMissingObject {
		t.Errorf("Class = %q, want %q (stderr: %q)", capture.Class, CaptureErrorMissingObject, capture.Stderr)
	}
	if capture.Retryable() {
		t.Error("a missing-object capture failure must not be retryable")
	}
	message := err.Error()
	if !strings.Contains(message, "git rev-parse:") {
		t.Errorf("error message %q does not name the git subcommand", message)
	}
	if !strings.Contains(message, "exit status 128") {
		t.Errorf("error message %q does not name the exit status", message)
	}
	if capture.Stderr == "" {
		t.Error("Stderr must not be empty for a real git failure")
	}
}

// TestIntegrationCaptureErrorLockedRepository is #5352's second required
// regression: an index.lock left by a concurrent (or crashed) git process
// must classify as CaptureErrorLocked and be reported retryable, unlike the
// missing-object case above.
func TestIntegrationCaptureErrorLockedRepository(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")

	lock := filepath.Join(repository, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte{}, 0o600); err != nil {
		t.Fatalf("plant index.lock: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })

	err := recoveryGit(context.Background(), repository, io.Discard, "add", "--all")
	if err == nil {
		t.Fatal("expected a failure against a locked index")
	}
	var capture *CaptureError
	if !errors.As(err, &capture) {
		t.Fatalf("error %v is not a *CaptureError", err)
	}
	if capture.Subcommand != "add" {
		t.Errorf("Subcommand = %q, want %q", capture.Subcommand, "add")
	}
	if capture.ExitCode != 128 {
		t.Errorf("ExitCode = %d, want 128", capture.ExitCode)
	}
	if capture.Class != CaptureErrorLocked {
		t.Errorf("Class = %q, want %q (stderr: %q)", capture.Class, CaptureErrorLocked, capture.Stderr)
	}
	if !capture.Retryable() {
		t.Error("a lock-contention capture failure must be retryable")
	}
	message := err.Error()
	if !strings.Contains(message, "git add:") {
		t.Errorf("error message %q does not name the git subcommand", message)
	}
	if !strings.Contains(strings.ToLower(message), "lock") {
		t.Errorf("error message %q does not mention the lock", message)
	}
}
