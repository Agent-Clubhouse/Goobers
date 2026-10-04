package worktree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRawGitOutputPreservesStdoutBytes(t *testing.T) {
	out, err := rawGitOutput(context.Background(), "", nil, "hash-object", "--stdin")
	if err != nil {
		t.Fatalf("rawGitOutput: %v", err)
	}
	if !bytes.HasSuffix(out, []byte("\n")) {
		t.Fatalf("rawGitOutput = %q, want trailing newline", out)
	}
	if bytes.HasSuffix(out, []byte("\r\n")) {
		t.Fatalf("rawGitOutput = %q, want git's raw LF unchanged", out)
	}
}

func TestGitCommandFailureOutputModes(t *testing.T) {
	dir := t.TempDir()
	before := filepath.Join(dir, "before.txt")
	after := filepath.Join(dir, "after.txt")
	if err := os.WriteFile(before, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(after, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"diff", "--no-index", "--", before, after}

	_, err := rawGitOutput(context.Background(), "", nil, args...)
	rawErr := commandError(t, err)
	if len(rawErr.output) != 0 {
		t.Fatalf("rawGitOutput failure output = %q, want stderr only", rawErr.output)
	}

	err = runGitWithEnv(context.Background(), "", nil, args...)
	combinedErr := commandError(t, err)
	if !bytes.Contains(combinedErr.output, []byte("-before")) || !bytes.Contains(combinedErr.output, []byte("+after")) {
		t.Fatalf("runGitWithEnv failure output = %q, want combined diff output", combinedErr.output)
	}
}

func commandError(t *testing.T, err error) *gitCommandError {
	t.Helper()
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) {
		t.Fatalf("error = %v (%T), want *gitCommandError", err, err)
	}
	return gitErr
}
