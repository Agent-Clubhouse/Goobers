package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file defines artifact passing for the stage contract: the ArtifactPointer
// (a journal-relative path + content digest), the ContextPointer union a stage
// receives as read-only input, and the resolution helpers that let a stage read
// an artifact back from the journal with its digest verified.
//
// Artifacts are how stages exchange anything larger than a scalar. Stage A writes
// bytes into the run journal and returns an ArtifactPointer; stage B receives that
// pointer (as a ContextPointer) and resolves it read-only, digest-verified. No
// stage ever hands another stage a live object — only a pointer (§2.4, §5).

// DigestAlgo is the only digest algorithm the contract commits to at V0.
const DigestAlgo = "sha256"

// ErrDigestMismatch is returned when an artifact's bytes do not match the digest
// recorded in its pointer — the artifact was changed or the pointer is stale.
var ErrDigestMismatch = errors.New("artifact digest mismatch")

// ErrPathEscape is returned when an artifact path would resolve outside the
// journal root (absolute path or ".." traversal). Artifact paths are always
// journal-relative and contained.
var ErrPathEscape = errors.New("artifact path escapes the journal root")

// ErrSymlinkEscape is returned when a path's real, symlink-resolved location
// lies outside root even though it lexically appeared contained (#120): a
// declared path (or one of its parent components) is a symlink whose target
// escapes root. Declared paths — a deterministic stage's resultFile, an
// agentic stage's artifactFile, a journal ArtifactPointer's Path — are
// untrusted input under SEC-047's threat model (the producer may be a
// prompt-injected agent), so a symlink is refused rather than followed:
// otherwise the executor would faithfully lift an arbitrary host file (e.g.
// ~/.ssh/id_rsa) into the journal as if it were the stage's own output.
var ErrSymlinkEscape = errors.New("path resolves outside root via a symlink")

// ArtifactPointer references a stage output stored in the run journal. It is the
// ONLY way stages exchange non-scalar data. The path is journal-relative (e.g.
// "artifacts/<stage>/diff.patch"); the digest pins the content so a resolve is
// tamper-evident and cross-run comparable (§4).
type ArtifactPointer struct {
	// Path is the journal-relative location of the artifact bytes. It must stay
	// within the journal root: no leading "/", no ".." traversal.
	Path string `json:"path"`
	// Digest is the content digest as "sha256:<64-hex>". It commits to the exact
	// bytes at Path (after journal-side redaction/scrubbing, per §4).
	Digest string `json:"digest"`
	// MediaType optionally categorizes the bytes (e.g. "text/x-patch",
	// "application/json"). Advisory; the digest is authoritative.
	MediaType string `json:"mediaType,omitempty"`
	// Size is the byte length of the artifact, when known. Advisory.
	Size int64 `json:"size,omitempty"`
}

// ContextPointer is one read-only input handed to a stage in its invocation. It
// is exactly one of an in-journal Artifact or an External reference; a stage
// consumes upstream work and input snapshots only through these — never through
// upstream result bodies.
type ContextPointer struct {
	// Name is the logical handle the stage refers to this input by.
	Name string `json:"name"`
	// Artifact points at an in-journal artifact. Mutually exclusive with External.
	Artifact *ArtifactPointer `json:"artifact,omitempty"`
	// External points at a resource outside the journal (e.g. an issue/PR URL).
	// Mutually exclusive with Artifact.
	External *ExternalRef `json:"external,omitempty"`
	// RunID names the run Artifact lives in, when it is NOT this stage's own
	// run (issue #103/T3: the Tutor's analyst resolving evidence for a
	// DIFFERENT run a cross-run telemetry query flagged). Empty (the default)
	// means Artifact is this run's own upstream output — the #121 same-run
	// path, unchanged and uncapability-gated. Non-empty crosses a real trust
	// boundary (another run's journal) and requires the journal:read
	// capability; internal/harness's materializeContext is what enforces
	// that and resolves Artifact against the OTHER run's journal root
	// instead of this run's own. Ignored when Artifact is nil.
	RunID string `json:"runId,omitempty"`
}

// ExternalRef points at a resource outside the run journal — an issue, a pull
// request, an arbitrary URL. It carries no content, only a locator; fetching it
// (and any trust decision about the content) is the stage's job.
type ExternalRef struct {
	// Kind categorizes the reference (e.g. "issue", "pull-request", "url").
	Kind string `json:"kind"`
	// URI locates the resource.
	URI string `json:"uri"`
	// Description is an optional human-facing label.
	Description string `json:"description,omitempty"`
}

// Digest computes the contract digest ("sha256:<hex>") of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestAlgo + ":" + hex.EncodeToString(sum[:])
}

// Validate reports whether the pointer is structurally well-formed: a contained
// journal-relative path and a syntactically valid sha256 digest. It does not
// touch the filesystem.
func (p ArtifactPointer) Validate() error {
	if _, err := containedPath("", p.Path); err != nil {
		return err
	}
	return validateDigest(p.Digest)
}

// Resolve reads the artifact bytes from journalRoot and verifies them against the
// pointer's digest. It is read-only and refuses any path that escapes journalRoot,
// lexically or via a symlink (#120). A digest mismatch returns ErrDigestMismatch.
func (p ArtifactPointer) Resolve(journalRoot string) ([]byte, error) {
	if err := validateDigest(p.Digest); err != nil {
		return nil, err
	}
	full, err := ResolveContainedPath(journalRoot, p.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read artifact %q: %w", p.Path, err)
	}
	if got := Digest(data); got != p.Digest {
		return nil, fmt.Errorf("%w: pointer %s, actual %s", ErrDigestMismatch, p.Digest, got)
	}
	return data, nil
}

// Verify is Resolve without returning the bytes: it confirms the artifact exists
// and its content still matches the pointer's digest.
func (p ArtifactPointer) Verify(journalRoot string) error {
	_, err := p.Resolve(journalRoot)
	return err
}

// WriteArtifact writes data into the run journal at relPath and returns a pointer
// to it. relPath is journal-relative and must stay within journalRoot. Parent
// directories are created as needed. The returned pointer's digest commits to the
// exact bytes written, so a later Resolve round-trips.
//
// It is the executor/runner's job to invoke journal-side redaction before calling
// this so the digest commits to scrubbed bytes (§4); WriteArtifact digests
// whatever it is given.
func WriteArtifact(journalRoot, relPath string, data []byte, mediaType string) (ArtifactPointer, error) {
	full, err := containedPath(journalRoot, relPath)
	if err != nil {
		return ArtifactPointer{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return ArtifactPointer{}, fmt.Errorf("create artifact dir: %w", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return ArtifactPointer{}, fmt.Errorf("write artifact %q: %w", relPath, err)
	}
	return ArtifactPointer{
		Path:      filepath.ToSlash(filepath.Clean(relPath)),
		Digest:    Digest(data),
		MediaType: mediaType,
		Size:      int64(len(data)),
	}, nil
}

// Validate reports whether the context pointer is well-formed: it must carry a
// name and exactly one of an Artifact or an External reference.
func (c ContextPointer) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("context pointer: name is required")
	}
	switch {
	case c.Artifact != nil && c.External != nil:
		return fmt.Errorf("context pointer %q: artifact and external are mutually exclusive", c.Name)
	case c.Artifact != nil:
		return c.Artifact.Validate()
	case c.External != nil:
		if strings.TrimSpace(c.External.URI) == "" {
			return fmt.Errorf("context pointer %q: external.uri is required", c.Name)
		}
		return nil
	default:
		return fmt.Errorf("context pointer %q: exactly one of artifact or external is required", c.Name)
	}
}

func validateDigest(d string) error {
	const prefix = DigestAlgo + ":"
	hexPart, ok := strings.CutPrefix(d, prefix)
	if !ok {
		return fmt.Errorf("artifact digest %q must be %s<hex>", d, prefix)
	}
	if len(hexPart) != 64 {
		return fmt.Errorf("artifact digest %q: expected 64 hex chars, got %d", d, len(hexPart))
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("artifact digest %q: not valid hex: %w", d, err)
	}
	return nil
}

// ResolveContainedPath is the single containment primitive every "read a
// declared, untrusted-relative path against a fixed root" call site must use
// instead of a bare filepath.Join + os.ReadFile (#120): a deterministic
// stage's declared resultFile, an agentic stage's declared artifactFile, and
// in-journal ArtifactPointer resolution all read this way.
//
// It first requires rel to be lexically contained (see containedPath: no
// absolute path, no ".." traversal — ErrPathEscape). It then resolves
// symlinks in both root and the joined path and requires the *resolved* path
// to still be contained within the *resolved* root, refusing a path that is
// lexically safe but is (or passes through) a symlink whose target escapes
// root (ErrSymlinkEscape).
//
// A target that does not yet exist surfaces as filepath.EvalSymlinks' own
// not-exist error (a *PathError satisfying errors.Is(err, fs.ErrNotExist)),
// so callers can keep distinguishing "missing" from "escapes" from a deeper
// I/O failure exactly as before this check existed.
func ResolveContainedPath(root, rel string) (string, error) {
	full, err := containedPath(root, rel)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	relBack, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q resolves to %q", ErrSymlinkEscape, rel, resolved)
	}
	return resolved, nil
}

// containedPath joins a journal-relative rel onto root and guarantees the result
// stays within root. rel must not be absolute or traverse above root. When root
// is empty the path is validated for containment without being made absolute
// (used by structural Validate, which does not touch the filesystem).
func containedPath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathEscape, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, rel)
	}
	if root == "" {
		return clean, nil
	}
	return filepath.Join(root, clean), nil
}
