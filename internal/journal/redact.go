package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNothingRedacted is returned by Redact when scrubbing the target blob
// changed nothing — the leaked material is not known to the run's scrubber.
// Register the secret with the scrubber before calling Redact.
var ErrNothingRedacted = errors.New("journal: nothing redacted (register the secret first)")

// Redact is the one sanctioned edit to an otherwise append-only journal: it
// replaces a leaked blob with a scrubbed copy and appends a redaction event
// recording the old→new digests, so even the exception leaves a trace (§4). This
// backs `goobers journal redact`.
//
// The caller must first register the leaked value with the run's scrubber (so it
// is caught here and never re-enters the journal). Redact re-reads the stored
// bytes, scrubs them, writes the scrubbed content, removes the leaked bytes from
// rest (SEC-041), and logs the remediation. It returns the new Ref.
func (r *Run) Redact(target Ref, reason string) (Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Ref{}, ErrClosed
	}
	oldPath := filepath.Join(r.dir, target.Path)
	raw, err := os.ReadFile(oldPath)
	if err != nil {
		return Ref{}, fmt.Errorf("journal: read blob to redact %q: %w", target.Path, err)
	}
	scrubbed := r.scrubber.Scrub(raw)
	newDigest := Digest(scrubbed)
	if newDigest == target.Digest {
		return Ref{}, ErrNothingRedacted
	}

	// Content-addressed artifacts move to the new digest's path; named blobs
	// (inputs) are rewritten in place so their friendly name is preserved.
	newRelPath := target.Path
	if isArtifactPath(target.Path) {
		if newRelPath, err = artifactPath(newDigest); err != nil {
			return Ref{}, err
		}
	}
	newRef, err := writeContentScrubbed(r.dir, newRelPath, scrubbed, newDigest)
	if err != nil {
		return Ref{}, fmt.Errorf("journal: write redacted blob: %w", err)
	}
	// Remove the leaked bytes from rest. If the path is unchanged (in-place
	// rewrite) writeContentScrubbed already overwrote them.
	if newRef.Path != target.Path {
		if err := os.Remove(oldPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Ref{}, fmt.Errorf("journal: remove leaked blob: %w", err)
		}
	}

	ev := Event{
		Type: EventRedaction,
		Name: target.Path,
		Ref:  &newRef,
		Redaction: &RedactionInfo{
			Target:    target.Path,
			OldDigest: target.Digest,
			NewDigest: newDigest,
			Reason:    reason,
		},
	}
	if err := r.append(ev); err != nil {
		return Ref{}, err
	}
	if err := r.checkpoint(); err != nil {
		return Ref{}, err
	}
	return newRef, nil
}

// isArtifactPath reports whether relPath is a content-addressed artifact blob
// (as opposed to a named input snapshot).
func isArtifactPath(relPath string) bool {
	return strings.HasPrefix(relPath, dirArtifacts+"/")
}
