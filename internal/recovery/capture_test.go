package recovery

import (
	"errors"
	"strings"
	"testing"
)

func TestGitSubcommandExtraction(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"plain verb", []string{"merge-base", "--all", "base", "HEAD"}, "merge-base"},
		{"leading -c pair", []string{"-c", "core.quotePath=true", "-c", "diff.suppressBlankEmpty=false", "diff", "--binary"}, "diff"},
		{"single -c pair before bundle", []string{"-c", "transfer.fsckObjects=true", "bundle", "unbundle", "staged"}, "bundle"},
		{"ls-files", []string{"ls-files", "--others", "--exclude-standard", "-z"}, "ls-files"},
		{"commit-tree", []string{"-c", "commit.gpgsign=false", "commit-tree", "tree-id", "-p", "parent"}, "commit-tree"},
		{"empty args", []string{}, "git"},
		{"only flags", []string{"-c", "a=b"}, "git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitSubcommand(tc.args); got != tc.want {
				t.Errorf("gitSubcommand(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestClassifyCaptureStderr(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		want   CaptureErrorClass
	}{
		{
			"unknown revision",
			"fatal: ambiguous argument 'deadbeef': unknown revision or path not in the working tree.",
			CaptureErrorMissingObject,
		},
		{
			"bad object",
			"fatal: bad object deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			CaptureErrorMissingObject,
		},
		{
			"not a valid object name",
			"fatal: not a valid object name 'HEAD'",
			CaptureErrorMissingObject,
		},
		{
			"index.lock present",
			"fatal: Unable to create '/repo/.git/index.lock': File exists.",
			CaptureErrorLocked,
		},
		{
			"another process running",
			"fatal: Another git process seems to be running in this repository",
			CaptureErrorLocked,
		},
		{
			"dubious ownership",
			"fatal: detected dubious ownership in repository at '/repo'",
			CaptureErrorUnsafeRepository,
		},
		{
			"unclassified",
			"fatal: some other failure entirely",
			CaptureErrorUnclassified,
		},
		{
			"empty stderr",
			"",
			CaptureErrorUnclassified,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCaptureStderr(tc.stderr); got != tc.want {
				t.Errorf("classifyCaptureStderr(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

func TestCaptureErrorClassRetryable(t *testing.T) {
	for _, tc := range []struct {
		class CaptureErrorClass
		want  bool
	}{
		{CaptureErrorLocked, true},
		{CaptureErrorMissingObject, false},
		{CaptureErrorUnsafeRepository, false},
		{CaptureErrorUnclassified, false},
	} {
		if got := tc.class.Retryable(); got != tc.want {
			t.Errorf("CaptureErrorClass(%q).Retryable() = %v, want %v", tc.class, got, tc.want)
		}
	}
}

func TestCaptureErrorMessageNamesSubcommandAndStderr(t *testing.T) {
	err := newCaptureError([]string{"merge-base", "--all", "base", "HEAD"}, 128,
		"fatal: unknown revision or path not in the working tree.", errors.New("exit status 128"))
	message := err.Error()
	if !strings.Contains(message, "git merge-base:") {
		t.Errorf("error %q does not name the git subcommand", message)
	}
	if !strings.Contains(message, "exit status 128") {
		t.Errorf("error %q does not name the exit status", message)
	}
	if !strings.Contains(message, "unknown revision") {
		t.Errorf("error %q does not carry the stderr diagnostic", message)
	}
	if err.Class != CaptureErrorMissingObject {
		t.Errorf("Class = %q, want %q", err.Class, CaptureErrorMissingObject)
	}
	if err.Retryable() {
		t.Error("a missing-object failure must not be classified retryable")
	}
	var target *CaptureError
	if !errors.As(err, &target) {
		t.Fatal("errors.As must find *CaptureError")
	}
	if err.Unwrap() == nil {
		t.Error("Unwrap must return the original cause")
	}
}

func TestCaptureErrorBoundsStderrToTail(t *testing.T) {
	long := strings.Repeat("x", captureStderrBound*3)
	err := newCaptureError([]string{"diff"}, 128, long+"tail-marker", errors.New("exit status 128"))
	if len(err.Stderr) > captureStderrBound {
		t.Fatalf("Stderr length = %d, want at most %d", len(err.Stderr), captureStderrBound)
	}
	if !strings.HasSuffix(err.Stderr, "tail-marker") {
		t.Fatalf("Stderr = %q, want the tail preserved", err.Stderr)
	}
}
