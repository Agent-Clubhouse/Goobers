package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

// Mode selects which completion contract a harness session is driven toward:
// a task producing a result envelope, or a reviewer gate producing a verdict.
// Both drive the same harness through the same invocation envelope
// (ARCHITECTURE.md §5); only the awaited completion artifact differs.
type Mode string

const (
	// ModeInvoke drives an agentic task to a result envelope (GBO-013).
	ModeInvoke Mode = "invoke"
	// ModeReview drives an agentic reviewer gate to a verdict.
	ModeReview Mode = "review"
)

// Default workspace-relative paths a harness is instructed to write its
// completion contract to.
const (
	DefaultResultPath  = ".goobers/result.json"
	DefaultVerdictPath = ".goobers/verdict.json"
)

// ErrNoCompletion is returned when a harness session ends without writing its
// declared completion file — the stage fails closed rather than reporting a
// partial success (GBO-013, GBO-014).
var ErrNoCompletion = errors.New("harness: no completion file written")

// ErrTimeout is returned when a harness session does not finish within its
// declared timeout.
var ErrTimeout = errors.New("harness: session timed out")

// ErrCanceled is returned when a harness session's context is canceled for a
// reason other than its own declared timeout (e.g. a hard-shutdown path
// cancels the ctx a caller passed in, distinct from runCtx's own deadline
// elapsing). Currently unreachable in practice — internal/runner's dispatch
// deliberately uses context.WithoutCancel as its drain contract, so no caller
// today ever cancels the ctx an Adapter/ProcessRunner receives — but kept
// distinct from ErrTimeout so a future hard-shutdown path is never mislabeled
// as a retryable timeout (#122).
var ErrCanceled = errors.New("harness: session canceled")

// ErrInvalidCompletion is returned when a harness's completion file exists but
// fails schema validation — fail closed the same as a missing file (GBO-013).
var ErrInvalidCompletion = errors.New("harness: completion file failed validation")

// RunRequest is everything an Adapter needs to drive one harness session in
// an already-prepared workspace (worktree creation and its BaseRef/branch are
// the runner's concern via internal/worktree, not this package's).
type RunRequest struct {
	// Mode selects the completion contract this session must satisfy.
	Mode Mode
	// Envelope is the invocation envelope (goal, context pointers, declared
	// capabilities, workspace path) the runner built for this stage.
	Envelope apiv1.InvocationEnvelope
	// Instructions is the goober's resolved instructions.md body (persona,
	// scope, done-criteria) — goober-level, not per-invocation.
	Instructions string
	// Workspace is the working directory the harness runs in — normally
	// Envelope.Workspace, threaded explicitly so tests can point it
	// elsewhere.
	Workspace string
	// CompletionPath is the workspace-relative path the harness must write
	// its result/verdict JSON to.
	CompletionPath string
	// Credentials is pre-scoped to Envelope.Capabilities: Token(ctx, cap)
	// fails closed for anything not declared for this stage.
	Credentials *credentials.Set
	// ContextPaths maps a resolved ContextPointer's Name to the
	// workspace-relative path its in-journal artifact bytes were
	// materialized to (#121) — populated for Artifact-backed pointers only;
	// External pointers carry no in-journal content to resolve. The prompt
	// renderer uses this to reference actual, readable file content instead
	// of an opaque pointer name.
	ContextPaths map[string]string
	// Timeout bounds the harness session; zero means no timeout.
	Timeout time.Duration
	// MaxTranscriptBytes caps the transcript a subprocess-based Adapter
	// retains in memory; non-positive means DefaultMaxTranscriptBytes (#245).
	MaxTranscriptBytes int64
}

// Outcome is what an Adapter hands back after a harness session ends.
type Outcome struct {
	// Payload is the raw bytes read from CompletionPath — unvalidated; the
	// Executor validates it against the mode's schema.
	Payload []byte
	// Transcript is the raw (unredacted) harness transcript, for the caller
	// to scrub and record as a journal span (GBO-020). Bounded at
	// MaxTranscriptBytes — a truncated transcript carries a trailing marker
	// (#245), never a silently cut-off blob.
	Transcript []byte
	// TranscriptTruncated reports whether Transcript was capped.
	TranscriptTruncated bool
	// TranscriptDroppedBytes is how many transcript bytes were discarded past
	// the cap (0 if TranscriptTruncated is false).
	TranscriptDroppedBytes int64
}

// Adapter is the harness-adapter seam (GBO-051): the only way an agentic
// stage drives an external coding agent. Implementations are security-critical
// (GBO-041) — an Adapter is trusted to materialize only the credentials its
// RunRequest.Credentials grants and never leak them outside the harness
// session's environment.
type Adapter interface {
	// Name identifies this adapter (e.g. "copilot-cli", "fake") in
	// diagnostics/spans. A Registry may expose it under a distinct goober
	// configuration name.
	Name() string
	// Preflight verifies the harness is installed and signed in, returning
	// an actionable error if not (GBO-011; wired into `goobers validate`).
	Preflight(ctx context.Context) error
	// Run drives one harness session to completion (or a failure) and
	// returns whatever it managed to capture. Run returning a non-nil error
	// means the stage did not complete; Run MUST NOT return a nil error
	// together with an empty/absent Payload — callers rely on
	// ErrNoCompletion for that distinction.
	Run(ctx context.Context, req RunRequest) (Outcome, error)
}

// readCompletion reads the harness's declared completion file back out of the
// workspace. A missing file is reported as ErrNoCompletion so callers can
// distinguish "the harness never finished" from other I/O failures — the
// fail-closed path every Adapter shares (GBO-013, GBO-014).
func readCompletion(workspace, relPath string) ([]byte, error) {
	full := filepath.Join(workspace, relPath)
	b, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoCompletion, relPath)
		}
		return nil, fmt.Errorf("harness: read completion file %s: %w", relPath, err)
	}
	return b, nil
}
