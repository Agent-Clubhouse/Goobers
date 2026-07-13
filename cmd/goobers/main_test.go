package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunNoArgs(t *testing.T) {
	code, _, stderr := runArgs(t)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("expected usage in stderr, got %q", stderr)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	code, _, stderr := runArgs(t, "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Fatalf("stderr = %q, want it to mention the unknown command", stderr)
	}
}

func TestRunHelp(t *testing.T) {
	code, stdout, _ := runArgs(t, "help")
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "goobers init") {
		t.Fatalf("expected help text, got %q", stdout)
	}
}

func TestInitThenValidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")

	code, stdout, stderr := runArgs(t, "init", root)
	if code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "initialized instance at") {
		t.Fatalf("init stdout = %q", stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "OK:") {
		t.Fatalf("validate stdout = %q", stdout)
	}

	// Re-running init is a no-op, not an error.
	code, stdout, _ = runArgs(t, "init", root)
	if code != 0 {
		t.Fatalf("second init: code = %d", code)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("second init stdout = %q", stdout)
	}
}

func TestValidateMissingInstance(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := runArgs(t, "validate", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestValidateTooManyArgs(t *testing.T) {
	code, _, _ := runArgs(t, "validate", "a", "b")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
