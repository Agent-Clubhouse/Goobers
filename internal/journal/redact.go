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
	// Redact is the one journal operation that reads, overwrites, and removes an
	// existing on-disk blob, so a target path that escaped the run directory
	// (via "..", or an absolute path) could read or destroy files outside the
	// journal. Every blob the journal writes is a plain relative path under the
	// run dir; anything else is rejected before we touch the filesystem.
	oldPath, err := containedBlobPath(r.dir, target.Path)
	if err != nil {
		return Ref{}, err
	}
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
	// Fail closed: verify the bytes actually at rest hash to the scrubbed digest
	// before we record success and delete the leaked original. Without this a
	// skipped or torn write (the blob never changed on disk) would be reported as
	// a successful redaction while the leaked bytes remained (SEC-041).
	if err := verifyBlobDigest(r.dir, newRef); err != nil {
		return Ref{}, err
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

// containedBlobPath resolves relPath against dir and guarantees the result stays
// inside dir. It rejects absolute paths and any path that climbs out via ".."
// so a caller-supplied Ref can never steer Redact's read/overwrite/remove at a
// file outside the run journal.
func containedBlobPath(dir, relPath string) (string, error) {
	if relPath == "" {
		return "", errors.New("journal: empty blob path")
	}
	clean := filepath.Clean(relPath)
	if rootedOrVolumeBoundBlobPath(relPath) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("journal: blob path %q escapes the run directory", relPath)
	}
	return filepath.Join(dir, clean), nil
}

func rootedOrVolumeBoundBlobPath(path string) bool {
	return filepath.IsAbs(path) ||
		filepath.VolumeName(path) != "" ||
		strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, `\`) ||
		(len(path) >= 2 && path[1] == ':' &&
			((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')))
}

// verifyBlobDigest re-reads the blob at ref.Path (relative to dir) and confirms
// its bytes hash to ref.Digest — the post-write proof that a redaction actually
// landed on disk, so Redact never reports success over unchanged or torn bytes.
func verifyBlobDigest(dir string, ref Ref) error {
	full, err := containedBlobPath(dir, ref.Path)
	if err != nil {
		return err
	}
	got, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("journal: verify redacted blob %q: %w", ref.Path, err)
	}
	if d := Digest(got); d != ref.Digest {
		return fmt.Errorf("journal: redacted blob %q has digest %s at rest, expected %s", ref.Path, d, ref.Digest)
	}
	return nil
}
