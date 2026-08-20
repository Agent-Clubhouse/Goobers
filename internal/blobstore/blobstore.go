// Package blobstore is the fleet-wide content-addressed store a distributed
// run's stages exchange artifacts through.
//
// WHY THIS EXISTS.
//
// A stage consumes prior work through ContextPointers, and the harness resolves
// each one with cp.Artifact.Resolve(journalRoot) — a read of
// <journalRoot>/<ref.Path> on the local filesystem. On a single runner that is
// correct and free: one process, one directory, every blob it needs is one it
// wrote. Across nodes it is neither. Stage 1 records into node A's run
// directory; stage 2 is polled by node B, whose directory for that run is
// empty; Resolve returns a missing file and the executor fails closed on an
// integrity fault. The placement machinery (engine.stageTaskQueue) can send a
// stage to another operating system, and the moment that stage needs anything
// its predecessor produced, it cannot read it.
//
// This is the same defect class as claims.json+flock: coordination through a
// filesystem two processes are assumed to share. It is worth naming as such,
// because the instinct is to fix it by making the filesystem actually shared —
// an RWX volume, an SMB mount — and that only moves the assumption somewhere
// harder to see.
//
// WHAT IT DOES INSTEAD.
//
// journal.Ref is already a sha256 digest of the scrubbed bytes. That makes the
// blob's identity independent of where it lives, which is the whole property a
// distributed data plane needs. So a worker's local run directory becomes a
// CACHE in front of a store the fleet shares:
//
//   - on record, write through to the store (Put);
//   - before a stage runs, fetch any pointer digest not already local (Get) and
//     write it where Resolve will look.
//
// Nothing downstream changes. Resolve still reads a local path. journal.Run,
// the projection, and the entire local runner are untouched — a tier-1 instance
// has no store configured and behaves byte-for-byte as before.
//
// WHAT IS DELIBERATELY NOT HERE.
//
// No cloud SDK. The interface is four methods over bytes and digests, and the
// only implementation shipped is a directory, because a directory is enough to
// prove the seam and honest about what it is. An object-store implementation
// (blob, S3, GCS) drops in behind the same interface without touching a caller,
// and until one exists the seam is the deliverable, not the backend.
package blobstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/platform/durability"
)

// ErrNotFound is returned by Get when the store holds no blob for a digest. It
// is distinguished from every other error on purpose: a missing blob is a
// legitimate cache outcome the caller may proceed past, whereas an unreadable
// store is not.
var ErrNotFound = errors.New("blobstore: blob not found")

// Store is a content-addressed blob store keyed by a journal.Ref digest
// ("sha256:<hex>").
//
// Implementations must treat a digest as the complete identity of its bytes:
// Put of an already-present digest is a no-op, never an overwrite, because
// two writers racing on the same digest are by construction writing the same
// content. That is what makes the store safe to share across a fleet with no
// locking at all — the property flock exists to provide, obtained instead by
// making the write idempotent.
type Store interface {
	// Get returns the blob's bytes, or ErrNotFound.
	Get(ctx context.Context, digest string) ([]byte, error)
	// Put stores data under digest. Storing a digest that is already present
	// succeeds without rewriting.
	Put(ctx context.Context, digest string, data []byte) error
	// Has reports whether the store holds the digest, so a caller can skip a
	// large Get it does not need.
	Has(ctx context.Context, digest string) (bool, error)
	// Describe names the store for diagnostics and journals. It must not
	// include credentials.
	Describe() string
}

// Dir is a Store backed by a directory.
//
// Whether that directory is node-local (a cache), a shared volume (a fleet
// store), or a mount over object storage is entirely the operator's business
// and invisible here. The layout is the journal's own — sha256/<aa>/<rest> —
// so a store directory and a journal's blob tree are structurally the same
// thing, and one can be seeded from the other by copying.
type Dir struct {
	Root string
}

// NewDir returns a directory-backed store rooted at root, creating it if
// needed. An empty root is an error rather than a silent cwd-relative store.
func NewDir(root string) (*Dir, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("blobstore: directory store needs a root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blobstore: create store root %q: %w", root, err)
	}
	return &Dir{Root: root}, nil
}

// Describe reports the store's root path.
func (d *Dir) Describe() string { return "dir:" + d.Root }

// Get reads the blob for digest.
func (d *Dir) Get(ctx context.Context, digest string) ([]byte, error) {
	path, err := d.pathFor(digest)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
		}
		return nil, fmt.Errorf("blobstore: read %s: %w", digest, err)
	}
	if actual := fmt.Sprintf("sha256:%x", sha256.Sum256(data)); actual != digest {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	return data, nil
}

// Has reports whether the blob is present.
func (d *Dir) Has(ctx context.Context, digest string) (bool, error) {
	path, err := d.pathFor(digest)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("blobstore: stat %s: %w", digest, err)
	}
}

// Put writes data under digest, or does nothing if it is already present.
//
// The write is staged and renamed rather than written in place. Two workers on
// two nodes can Put the same digest concurrently — same bytes, by definition —
// and a reader must never observe a half-written blob whose digest no longer
// commits to its contents. Rename is atomic within a filesystem, and syncing
// the staged file and parent directory makes the publication crash-durable.
func (d *Dir) Put(ctx context.Context, digest string, data []byte) error {
	path, err := d.pathFor(digest)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := d.Get(ctx, digest); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("blobstore: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return fmt.Errorf("blobstore: stage %s: %w", digest, err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("blobstore: write %s: %w", digest, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("blobstore: sync %s: %w", digest, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blobstore: close %s: %w", digest, err)
	}
	if err := durability.ReplaceFile(staged, path); err != nil {
		// A concurrent Put of the same digest already landed. Same bytes, so
		// this is success, not a conflict — and treating it as one is what
		// keeps the store lock-free.
		if _, getErr := d.Get(ctx, digest); getErr == nil {
			return nil
		}
		return fmt.Errorf("blobstore: publish %s: %w", digest, err)
	}
	if err := durability.SyncDir(dir); err != nil {
		return fmt.Errorf("blobstore: sync directory for %s: %w", digest, err)
	}
	return nil
}

// pathFor maps a digest to its path, rejecting anything that is not a plain
// sha256 hex digest. This is a security boundary, not a formatting nicety: a
// digest reaches here from a ContextPointer, which travels through Temporal
// history from an upstream stage, and an unvalidated one would be a path
// traversal with a remote origin (SEC-047's threat model, the same reason
// ValidRunID exists).
func pathForDigest(digest string) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return "", fmt.Errorf("blobstore: digest %q is not sha256-prefixed", digest)
	}
	if len(hex) != 64 {
		return "", fmt.Errorf("blobstore: digest %q is not 64 hex characters", digest)
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("blobstore: digest %q is not lowercase hex", digest)
		}
	}
	return filepath.Join("sha256", hex[:2], hex[2:]), nil
}

func (d *Dir) pathFor(digest string) (string, error) {
	rel, err := pathForDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(d.Root, rel), nil
}
