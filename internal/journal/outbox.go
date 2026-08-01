package journal

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// OutboxFile is one declared file exported into a run's durable,
// path-preserving outbox namespace (#1552) — distinct from the rest of
// artifacts/, which is content-addressed by digest: the relative path is
// preserved so a declared export stays browsable by name.
type OutboxFile struct {
	// RelPath is the file's path relative to the exporting stage's outbox
	// root. Never trust a caller's own validation of this path as
	// sufficient: it was checked (if at all) against a different root — the
	// stage workspace, not the outbox destination — so ExportOutbox always
	// re-validates containment itself against the outbox root before
	// writing anything.
	RelPath string
	Data    []byte
}

// MaxOutboxFilesPerAttempt and MaxOutboxBytesPerAttempt bound one stage
// attempt's outbox export (#1552). Declared paths are workflow-author
// controlled, but what they resolve to at runtime is stage-output
// controlled — an attacker-influenced (e.g. prompt-injected) agentic stage
// could otherwise grow a legitimately declared directory without bound.
// Both limits are aggregated across the entire batch passed to one
// ExportOutbox/ExportBranchOutbox call and enforced before any file in the
// batch is written: a batch that would exceed either limit is rejected in
// full, never silently truncated to fit. This is the single enforcement
// chokepoint every caller (initial dispatch, resume, rerun) shares — there
// is no caller-side limit to bypass by taking a different code path.
const (
	MaxOutboxFilesPerAttempt = 200
	MaxOutboxBytesPerAttempt = 64 << 20 // 64 MiB
)

// ExportOutbox durably exports files declared by a stage attempt into
// runs/<id>/artifacts/outbox/<stage>/attempt-<N>/<relative path>, scrubbing
// each and recording an artifact.recorded event per file before a single
// checkpoint. Every path is re-validated for containment against the
// outbox root regardless of any validation the caller already performed
// against the stage workspace (#1552's prior escalation: a "recovery"
// code path that skipped re-validation permitted traversal). An empty
// files slice is a no-op so stages that declare no outbox are unaffected.
func (r *Run) ExportOutbox(stage string, attempt int, class AttemptClass, files []OutboxFile) ([]Ref, error) {
	return r.exportOutbox(0, stage, attempt, class, files)
}

// ExportBranchOutbox is ExportOutbox with explicit parallel-branch
// attribution, mirroring RecordBranchStageArtifact.
func (r *Run) ExportBranchOutbox(branch int, stage string, attempt int, class AttemptClass, files []OutboxFile) ([]Ref, error) {
	return r.exportOutbox(branch, stage, attempt, class, files)
}

func (r *Run) exportOutbox(branch int, stage string, attempt int, class AttemptClass, files []OutboxFile) ([]Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if len(files) == 0 {
		return nil, nil
	}
	if err := validOutboxStage(stage); err != nil {
		return nil, err
	}
	if attempt < 1 {
		return nil, fmt.Errorf("journal: outbox export for stage %q requires attempt >= 1, got %d", stage, attempt)
	}
	if len(files) > MaxOutboxFilesPerAttempt {
		return nil, fmt.Errorf(
			"journal: outbox export for stage %q attempt %d declares %d files, exceeds the %d-file limit",
			stage, attempt, len(files), MaxOutboxFilesPerAttempt,
		)
	}

	type prepared struct {
		relPath string // sanitized, relative to the outbox root
		dest    string // journal-relative destination path
		data    []byte
	}
	batch := make([]prepared, 0, len(files))
	seen := make(map[string]bool, len(files))
	var total int64
	for _, f := range files {
		relPath, err := cleanOutboxRelPath(f.RelPath)
		if err != nil {
			return nil, fmt.Errorf("journal: outbox file %q: %w", f.RelPath, err)
		}
		if seen[relPath] {
			return nil, fmt.Errorf("journal: outbox export for stage %q attempt %d declares %q more than once", stage, attempt, relPath)
		}
		seen[relPath] = true
		scrubbed := r.scrubber.Scrub(f.Data)
		total += int64(len(scrubbed))
		if total > MaxOutboxBytesPerAttempt {
			return nil, fmt.Errorf(
				"journal: outbox export for stage %q attempt %d exceeds the %d-byte aggregate limit",
				stage, attempt, MaxOutboxBytesPerAttempt,
			)
		}
		dest := path.Join(dirArtifacts, dirOutbox, stage, fmt.Sprintf("attempt-%d", attempt), relPath)
		if _, err := containedBlobPath(r.dir, dest); err != nil {
			return nil, fmt.Errorf("journal: outbox destination for %q: %w", f.RelPath, err)
		}
		batch = append(batch, prepared{relPath: relPath, dest: dest, data: scrubbed})
	}

	refs := make([]Ref, 0, len(batch))
	for _, p := range batch {
		digest := Digest(p.data)
		ref, err := writeContentScrubbed(r.dir, p.dest, p.data, digest)
		if err != nil {
			return nil, fmt.Errorf("journal: write outbox file %q: %w", p.relPath, err)
		}
		ref.Integrity = apiv1.IntegrityDerived
		refs = append(refs, ref)
		ev := Event{
			Type: EventArtifactRecorded, Branch: branch, Stage: stage, Attempt: attempt, AttemptClass: class,
			Name: path.Join(dirOutbox, p.relPath), Ref: &ref, Integrity: ref.Integrity,
		}
		if err := r.append(ev); err != nil {
			return nil, fmt.Errorf("journal: record outbox file %q: %w", p.relPath, err)
		}
	}
	if err := r.checkpoint(); err != nil {
		return nil, err
	}
	return refs, nil
}

// cleanOutboxRelPath sanitizes a declared outbox-relative path: it must be
// relative, non-empty, and must not climb above the outbox root via "..".
// It does not touch the filesystem — ExportOutbox's containedBlobPath check
// on the joined destination is the actual containment guarantee; this is a
// cheap first-pass rejection of the obviously unsafe shapes.
func cleanOutboxRelPath(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty path")
	}
	if rootedOrVolumeBoundBlobPath(rel) {
		return "", errors.New("path must be relative")
	}
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path must stay within the outbox root")
	}
	return clean, nil
}

// validOutboxStage reports whether stage is safe to use as a single path
// segment in the outbox destination path. Stage names originate from the
// workflow definition, not runtime data, but every segment of an outbox
// destination is re-checked here rather than assuming an upstream compiler
// guarantee holds.
func validOutboxStage(stage string) error {
	if stage == "" {
		return errors.New("journal: outbox export requires a stage name")
	}
	if stage == "." || stage == ".." || strings.ContainsAny(stage, `/\`) {
		return fmt.Errorf("journal: outbox export stage name %q is not a safe path segment", stage)
	}
	return nil
}
