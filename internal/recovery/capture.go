package recovery

import (
	"fmt"
	"strings"
)

// captureStderrBound caps how much of a failing git command's stderr this
// package retains. Recovery's git subprocesses are all local (no network
// transport, GIT_* stripped from the child environment, safe.directory
// pinned to the exact resolved repository), so stderr here cannot carry a
// remote credential the way a clone/fetch failure could. The bound exists
// anyway: it is cheap insurance against a pathological message (a giant path
// list, a corrupt-object dump) filling a journal event, and it is what keeps
// this evidence journal-sized instead of unbounded.
const captureStderrBound = 4096

// CaptureErrorClass names the operator-actionable reason a recovery git
// subprocess exited non-zero. Git's own exit code is uniform (128 covers a
// missing object, a locked repository, and a dubious-ownership refusal
// alike), so this is derived from stderr text, not the exit code.
type CaptureErrorClass string

const (
	// CaptureErrorUnclassified is the zero value: a failure no rule
	// recognized. It is the default deliberately, so an unrecognized message
	// is reported as unclassified rather than mislabeled as one of the
	// specific causes below.
	CaptureErrorUnclassified CaptureErrorClass = ""
	// CaptureErrorMissingObject means git could not resolve a ref or object
	// this operation needed (an unknown revision, a bad or missing object).
	// Retrying reproduces the identical failure: the referenced state does
	// not exist and retrying cannot make it exist.
	CaptureErrorMissingObject CaptureErrorClass = "missing-object"
	// CaptureErrorLocked means another git process (or process's crash
	// residue) currently holds a lock this operation needed — most commonly
	// index.lock. This is transient contention, not a permanent fault: the
	// lock is released when its holder exits or its stale file is cleared.
	CaptureErrorLocked CaptureErrorClass = "locked"
	// CaptureErrorUnsafeRepository means git refused to operate on the
	// repository because ownership looks unsafe (git's "dubious ownership"
	// / "detected dubious ownership" refusal). This is a host/configuration
	// fault; retrying identically reproduces it.
	CaptureErrorUnsafeRepository CaptureErrorClass = "unsafe-repository"
)

// Retryable reports whether an identical retry of the same git operation can
// plausibly succeed. Only lock contention is: a missing object or an
// unsafe-repository refusal will fail again exactly the same way, and a
// caller that retries those anyway only delays reporting the real cause.
func (c CaptureErrorClass) Retryable() bool {
	return c == CaptureErrorLocked
}

// CaptureError is the recovery package's typed git subprocess failure. It
// carries what a bare exit code discards: which git subcommand ran, its
// bounded stderr tail, and the failure class derived from that stderr. Every
// existing "capture recovery patch: %w" / "capture recovery index: %w" style
// wrap therefore carries this evidence automatically, since %w preserves the
// chain for both errors.As and errors.Is.
type CaptureError struct {
	// Subcommand is the git verb this operation ran (merge-base, ls-files,
	// add, write-tree, commit-tree, bundle, diff, ...), not the full
	// argument list — the verb is what an operator needs to know which
	// recovery step failed.
	Subcommand string
	// Stderr is the command's captured stderr, bounded to captureStderrBound
	// bytes and keeping the TAIL (git's most specific diagnostic line is
	// usually last).
	Stderr string
	// ExitCode is the process's exit code, or -1 if it could not be
	// determined (the process never started, or was signaled).
	ExitCode int
	// Class is the operator-actionable failure class derived from Stderr.
	Class CaptureErrorClass
	cause error
}

// Retryable reports whether c's class makes an identical retry worth
// attempting.
func (c *CaptureError) Retryable() bool { return c.Class.Retryable() }

func (c *CaptureError) Error() string {
	if c.Stderr == "" {
		return fmt.Sprintf("git %s: exit status %d", c.Subcommand, c.ExitCode)
	}
	return fmt.Sprintf("git %s: exit status %d: %s", c.Subcommand, c.ExitCode, c.Stderr)
}

func (c *CaptureError) Unwrap() error { return c.cause }

// boundedTail returns the last n bytes of s, trimmed of surrounding
// whitespace so a trailing newline does not visually pad every message.
func boundedTail(s string, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return strings.TrimSpace(s)
}

// gitSubcommand extracts the verb a recoveryGit call actually ran, skipping
// the leading global options recovery callers pass before it ("-c",
// "key=value" pairs; any other "-"-prefixed flag). Every recoveryGit call
// site names its verb this way (merge-base, ls-files, add, write-tree,
// commit-tree, bundle, diff, ...), so this is a small, stable extraction
// rather than a full argument parser.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" {
			i++ // skip the "key=value" that follows
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return "git"
}

// classifyCaptureStderr derives a CaptureErrorClass from a git subprocess's
// stderr text. Matches are stable, low-level substrings from git's own
// messages (both English wording; git does not localize by default), chosen
// to survive across git versions and to work identically on Windows, where
// path separators and the exact lock-directory prose can differ but these
// fragments do not.
func classifyCaptureStderr(stderr string) CaptureErrorClass {
	message := strings.ToLower(stderr)
	switch {
	case containsAny(message,
		"unknown revision",
		"bad object",
		"bad revision",
		"not a valid object name",
		"could not find",
		"missing object",
		"needed a single revision"):
		return CaptureErrorMissingObject
	case containsAny(message,
		"index.lock",
		".lock':",
		"unable to create",
		"another git process seems to be running"):
		return CaptureErrorLocked
	case containsAny(message,
		"detected dubious ownership",
		"unsafe repository",
		"dubious ownership"):
		return CaptureErrorUnsafeRepository
	default:
		return CaptureErrorUnclassified
	}
}

func containsAny(haystack string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(haystack, fragment) {
			return true
		}
	}
	return false
}

// newCaptureError builds the typed failure for one recoveryGit invocation.
func newCaptureError(args []string, exitCode int, stderr string, cause error) *CaptureError {
	bounded := boundedTail(stderr, captureStderrBound)
	return &CaptureError{
		Subcommand: gitSubcommand(args),
		Stderr:     bounded,
		ExitCode:   exitCode,
		Class:      classifyCaptureStderr(bounded),
		cause:      cause,
	}
}
