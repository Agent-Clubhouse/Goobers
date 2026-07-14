package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ContextResolver exposes the run journal's root directory so the executor
// can resolve a stage's declared ContextPointers into its workspace before
// invocation (#121), via apiv1.ArtifactPointer.Resolve — already
// containment/symlink-safe (#120). Satisfied by (*internal/journal.Run).Dir
// without this package taking on journal's full durability/event-log
// machinery, mirroring how SpanRecorder/ArtifactRecorder narrow to one
// method each.
type ContextResolver interface {
	Dir() string
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
// External (non-artifact) pointers are left alone: they carry no in-journal
// content to resolve, and the prompt already surfaces their URI directly.
//
// A resolve failure here is NOT a normal stage failure the way a missing
// declared output file is (#120's ErrDeclaredArtifactMissing/
// ErrDeclaredArtifactPathEscape) — it means an *upstream* artifact this
// stage was promised is missing or tampered, an integrity fault in the run
// itself rather than something this stage's own declaration got wrong — so
// it propagates as a hard executor error (fail closed, GBO-013/014).
func (e *Executor) materializeContext(env apiv1.InvocationEnvelope) (map[string]string, error) {
	if len(env.ContextPointers) == 0 {
		return nil, nil
	}
	paths := make(map[string]string, len(env.ContextPointers))
	for i, cp := range env.ContextPointers {
		if cp.Artifact == nil {
			continue
		}
		data, err := cp.Artifact.Resolve(e.contextResolver.Dir())
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
