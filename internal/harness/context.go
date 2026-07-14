package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
)

// ContextResolver exposes the run journal's root directory so the executor
// can resolve a stage's declared ContextPointers into its workspace before
// invocation (#121), via apiv1.ArtifactPointer.Resolve — already
// containment/symlink-safe (#120). Satisfied by (*internal/journal.Run).Dir
// without this package taking on journal's full durability/event-log
// machinery, mirroring how SpanRecorder/ArtifactRecorder narrow to one
// method each.
//
// RunsDir additionally exposes the instance's runs directory — the parent
// of every run's own journal directory — so a capability-gated cross-run
// ContextPointer (ContextPointer.RunID set, issue #103/T3) can be resolved
// against a DIFFERENT run's journal root without this package needing to
// know the instance layout beyond "runs live under this one directory."
// journal.Run itself has no notion of sibling runs, so production wiring
// pairs it with the instance layout's RunsDir (see NewContextResolver).
type ContextResolver interface {
	Dir() string
	RunsDir() string
}

// runContextResolver is the concrete ContextResolver production wiring
// constructs: a per-run Dir() (structurally satisfied by *journal.Run, #121)
// paired with the instance's RunsDir (#103/T3). Kept in this package rather
// than cmd/goobers so the pairing lives next to the interface it satisfies.
type runContextResolver struct {
	run     interface{ Dir() string }
	runsDir string
}

// NewContextResolver builds a ContextResolver from any per-run type exposing
// Dir() (namely *journal.Run) plus the instance's runs directory.
func NewContextResolver(run interface{ Dir() string }, runsDir string) ContextResolver {
	return runContextResolver{run: run, runsDir: runsDir}
}

func (r runContextResolver) Dir() string     { return r.run.Dir() }
func (r runContextResolver) RunsDir() string { return r.runsDir }

// ErrJournalReadRequired is returned when a ContextPointer names another
// run (RunID set) but the invoking stage did not declare the journal:read
// capability — fail-closed per SEC-042/SEC-044's pattern (compile-time
// admission is a backstop, not the only check; this is the runtime one,
// mirroring internal/telemetry/query's HasCapability shape for a
// non-credentialed capability).
var ErrJournalReadRequired = errors.New("harness: journal:read capability required to resolve a cross-run context pointer")

// ErrInvalidRunID is returned when a ContextPointer's RunID is not a single,
// safe path component — it must never itself be used to escape RunsDir
// (e.g. "../../etc" or an absolute path smuggled in via a compromised or
// prompt-injected upstream artifact, SEC-047's threat model).
var ErrInvalidRunID = errors.New("harness: invalid run id")

// hasCapability reports whether cap is present in declared — the same
// linear-scan shape internal/telemetry/query.HasCapability uses; declared
// lists are always small (a handful of capabilities per task).
func hasCapability(declared []string, cap capability.Capability) bool {
	for _, c := range declared {
		if c == string(cap) {
			return true
		}
	}
	return false
}

// contextDir is the workspace-relative directory declared context artifacts
// are materialized into, ahead of every harness session that declares any
// ContextPointers.
const contextDir = ".goobers/context"

// unsafeContextChar matches anything outside a conservative filesystem-safe
// character set. ContextPointer.Name traces back to workflow/task
// definitions and is not assumed to be a safe path component — the same
// containment lesson #120 applied to reads, applied here to the write side.
var unsafeContextChar = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// safeContextFilename derives a filesystem-safe filename for a context
// pointer at position index in the envelope's ContextPointers, from its
// Name. The index prefix guarantees uniqueness even if sanitizing two
// different names collides.
func safeContextFilename(index int, name string) string {
	safe := unsafeContextChar.ReplaceAllString(name, "_")
	if safe == "" {
		safe = "context"
	}
	return fmt.Sprintf("%02d-%s", index, safe)
}

// materializeContext resolves every Artifact-backed entry in
// env.ContextPointers (digest-verified, containment/symlink-safe) into
// workspace/.goobers/context/, so an agentic stage's own tools (file read,
// grep, ...) can actually reach upstream artifact content instead of only
// ever seeing an opaque pointer name (#121, ARCHITECTURE.md §5). It returns
// the workspace-relative path resolved for each pointer, keyed by
// ContextPointer.Name, for the prompt renderer to reference.
//
// A pointer with RunID set names a DIFFERENT run's journal (#103/T3, e.g.
// the Tutor's analyst resolving evidence for a run a cross-run telemetry
// query flagged) rather than this stage's own upstream output. That crosses
// a real trust boundary — another run's journal, not just this run's own
// already-trusted upstream artifacts — so it is refused fail-closed
// (ErrJournalReadRequired) unless env.Capabilities declares journal:read,
// and RunID itself is validated as a single safe path segment
// (ErrInvalidRunID) before it is ever joined onto RunsDir, since RunID can
// originate from artifact CONTENT an upstream stage produced (e.g. a
// candidate-findings JSON a prompt-injected agent could have tampered with,
// SEC-047's threat model) rather than from the trusted workflow definition
// itself. Once past both checks, resolution is identical to the same-run
// path: apiv1.ArtifactPointer.Resolve against the target run's journal root,
// still digest-verified and containment/symlink-safe (#120).
//
// External (non-artifact) pointers are left alone: they carry no in-journal
// content to resolve, and the prompt already surfaces their URI directly.
//
// A resolve failure here is NOT a normal stage failure the way a missing
// declared output file is (#120's ErrDeclaredArtifactMissing/
// ErrDeclaredArtifactPathEscape) — it means an *upstream* artifact this
// stage was promised is missing or tampered, an integrity fault in the run
// itself rather than something this stage's own declaration got wrong — so
// it propagates as a hard executor error (fail closed, GBO-013/014). The
// same treatment applies to a refused cross-run pointer: an undeclared
// capability or an invalid RunID on a pointer the stage was HANDED (not one
// it invented) is still an integrity fault in what upstream promised it,
// not a business failure this stage caused.
func (e *Executor) materializeContext(env apiv1.InvocationEnvelope) (map[string]string, error) {
	if len(env.ContextPointers) == 0 {
		return nil, nil
	}
	paths := make(map[string]string, len(env.ContextPointers))
	for i, cp := range env.ContextPointers {
		if cp.Artifact == nil {
			continue
		}
		journalRoot := e.contextResolver.Dir()
		if cp.RunID != "" {
			if !hasCapability(env.Capabilities, capability.JournalRead) {
				return nil, fmt.Errorf("harness: resolve context pointer %q: %w", cp.Name, ErrJournalReadRequired)
			}
			if !apiv1.ValidRunID(cp.RunID) {
				return nil, fmt.Errorf("harness: resolve context pointer %q: %w: %q", cp.Name, ErrInvalidRunID, cp.RunID)
			}
			journalRoot = filepath.Join(e.contextResolver.RunsDir(), cp.RunID)
		}
		data, err := cp.Artifact.Resolve(journalRoot)
		if err != nil {
			return nil, fmt.Errorf("harness: resolve context pointer %q: %w", cp.Name, err)
		}
		relPath := filepath.Join(contextDir, safeContextFilename(i, cp.Name))
		full := filepath.Join(env.Workspace, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, fmt.Errorf("harness: prepare context dir: %w", err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return nil, fmt.Errorf("harness: write context artifact %q: %w", cp.Name, err)
		}
		paths[cp.Name] = relPath
	}
	return paths, nil
}
