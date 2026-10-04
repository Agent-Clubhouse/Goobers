package childpod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// ParentBlobs owns pre-child custody under the parent journal's retention root.
// The ordinary journal retention/active-family holds own its lifetime. This is
// deliberately outside artifacts/: ordinary artifact reads cannot discover a
// carrier from a blob digest. The authenticated HTTP adapter supplies Identity
// and verifies active/teardown authority before using this store.
type ParentBlobs struct {
	RunDir   string
	Identity journal.RunIdentity
}

// Mandatory per-run bounds include blob bytes and ownership-index overhead.
const (
	MaxParentCustodyBytes  = 64 << 20
	MaxParentCustodyBlobs  = 512
	parentCustodyDirectory = "isolated-parent-custody"
)

// Describe returns a credential-free backend name.
func (s ParentBlobs) Describe() string { return "bounded-parent-custody" }

func (s ParentBlobs) directory(ctx context.Context, create bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.RunDir == "" || s.Identity.Child != nil || s.Identity.RunID == "" {
		return "", fmt.Errorf("parent custody owner unavailable")
	}
	reader, err := journal.OpenReadOnly(s.RunDir)
	if err != nil {
		return "", err
	}
	id, err := reader.Identity()
	if err != nil {
		return "", err
	}
	if id.Child != nil || id.RunID != s.Identity.RunID || id.Gaggle != s.Identity.Gaggle || id.InstanceID != s.Identity.InstanceID || id.WorkflowDigest != s.Identity.WorkflowDigest || id.GooberDigest != s.Identity.GooberDigest || id.ConfigGeneration != s.Identity.ConfigGeneration {
		return "", fmt.Errorf("parent custody differs from pinned run")
	}
	dir := filepath.Join(s.RunDir, parentCustodyDirectory)
	if create {
		if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", blobstore.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("parent custody directory is not owned")
	}
	return dir, nil
}

// Get reads verified bytes only while the pinned parent journal exists.
func (s ParentBlobs) Get(ctx context.Context, digest string) ([]byte, error) {
	if !blobstore.ValidDigest(digest) {
		return nil, blobstore.ErrNotFound
	}
	dir, err := s.directory(ctx, false)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readParentBlob(ctx, root, digest)
}

func readParentBlob(ctx context.Context, root *os.Root, digest string) ([]byte, error) {
	f, err := safeopen.OpenRegularInRoot(root, strings.TrimPrefix(digest, "sha256:"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, safeopen.ErrNotRegular) {
		return nil, blobstore.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxContractBytes {
		return nil, blobstore.ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxContractBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxContractBytes {
		return nil, blobstore.ErrTooLarge
	}
	if journal.Digest(data) != digest {
		return nil, fmt.Errorf("parent custody digest mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

// Has checks digest-verified presence in this parent run.
func (s ParentBlobs) Has(ctx context.Context, digest string) (bool, error) {
	_, err := s.Get(ctx, digest)
	if errors.Is(err, blobstore.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Put serializes quota reservation and durable publication across writers.
func (s ParentBlobs) Put(ctx context.Context, digest string, data []byte) (retErr error) {
	if len(data) > MaxContractBytes || !blobstore.ValidDigest(digest) || journal.Digest(data) != digest {
		return fmt.Errorf("parent custody digest or byte bound exceeded")
	}
	dir, err := s.directory(ctx, true)
	if err != nil {
		return err
	}
	held, err := parentCustodyLock(ctx, filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, held.Release()) }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	if _, err = readParentBlob(ctx, root, digest); err == nil {
		return nil
	} else if !errors.Is(err, blobstore.ErrNotFound) {
		return err
	}
	count, size, err := parentCustodySize(root)
	if err != nil {
		return err
	}
	if count >= MaxParentCustodyBlobs || size+int64(len(data))+1024 > MaxParentCustodyBytes {
		return fmt.Errorf("parent custody quota exceeded")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return durability.WriteFileAtomic(filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")), data, 0600)
}

func parentCustodyLock(ctx context.Context, path string) (*lock.Handle, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		held, err := lock.TryAcquire(path)
		if !errors.Is(err, lock.ErrHeld) {
			return held, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func parentCustodySize(root *os.Root) (int, int64, error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(MaxParentCustodyBlobs + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, err
	}
	var count int
	var size int64
	for _, entry := range entries {
		if entry.Name() == ".lock" {
			continue
		}
		// Include crash-left temporary files in the bound; they cannot be ignored
		// to accumulate quota-free bytes. Unexpected local edits fail closed.
		info, err := entry.Info()
		if err != nil {
			return 0, 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, 0, fmt.Errorf("unexpected parent custody entry")
		}
		count++
		size += info.Size() + 1024
	}
	return count, size, nil
}

// ParentAttemptBlobs is the HTTP/returned-artifact view for one signed parent
// contract. A new upload establishes ownership only after its supplied bytes
// match the content digest. A hash alone never grants a sibling read.
type ParentAttemptBlobs struct {
	Store          ParentBlobs
	ContractDigest string
}

// Describe returns a credential-free backend name.
func (s ParentAttemptBlobs) Describe() string { return "bounded-parent-attempt-custody" }

// Get requires this exact contract to own the requested digest.
func (s ParentAttemptBlobs) Get(ctx context.Context, digest string) ([]byte, error) {
	if !blobstore.ValidDigest(s.ContractDigest) || !blobstore.ValidDigest(digest) {
		return nil, blobstore.ErrNotFound
	}
	dir, err := s.Store.directory(ctx, false)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	marker, err := safeopen.OpenRegularInRoot(root, parentReadMarker(s.ContractDigest, digest))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, safeopen.ErrNotRegular) {
		return nil, blobstore.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = marker.Close(); err != nil {
		return nil, err
	}
	return readParentBlob(ctx, root, digest)
}

// Put grants attempt ownership only after storing verified supplied bytes.
func (s ParentAttemptBlobs) Put(ctx context.Context, digest string, data []byte) error {
	if !blobstore.ValidDigest(s.ContractDigest) {
		return fmt.Errorf("parent contract unavailable")
	}
	if err := s.Store.Put(ctx, digest, data); err != nil {
		return err
	}
	return s.Store.AllowContractRead(ctx, s.ContractDigest, digest)
}

// Has checks presence only within the signed attempt scope.
func (s ParentAttemptBlobs) Has(ctx context.Context, digest string) (bool, error) {
	_, err := s.Get(ctx, digest)
	if errors.Is(err, blobstore.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// AllowContractRead is a trusted-host operation used only to seed immutable
// inputs. HTTP callers get ParentAttemptBlobs, not this authority method.
func (s ParentBlobs) AllowContractRead(ctx context.Context, contractDigest, digest string) (retErr error) {
	if !blobstore.ValidDigest(contractDigest) || !blobstore.ValidDigest(digest) {
		return fmt.Errorf("parent contract digest unavailable")
	}
	data, err := s.Get(ctx, contractDigest)
	if err != nil {
		return err
	}
	c, err := DecodeContract(data, contractDigest)
	if err != nil || c.ParentOrigin == nil {
		return errors.Join(fmt.Errorf("parent contract unavailable"), err)
	}
	if c.Identity.RunID != s.Identity.RunID || c.Identity.Gaggle != s.Identity.Gaggle {
		return fmt.Errorf("parent contract belongs to another owner")
	}
	if _, err = s.Get(ctx, digest); err != nil {
		return err
	}
	dir, err := s.directory(ctx, false)
	if err != nil {
		return err
	}
	held, err := parentCustodyLock(ctx, filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, held.Release()) }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	name := parentReadMarker(contractDigest, digest)
	existing, err := safeopen.OpenRegularInRoot(root, name)
	if err == nil {
		return existing.Close()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	count, size, err := parentCustodySize(root)
	if err != nil {
		return err
	}
	if count >= MaxParentCustodyBlobs || size+1024 > MaxParentCustodyBytes {
		return fmt.Errorf("parent custody index quota exceeded")
	}
	return durability.WriteFileAtomic(filepath.Join(dir, name), nil, 0600)
}

// BindContract seeds exactly the input contract, kit and declared context. It
// runs before the worker can mint a token or start the pod.
func (s ParentBlobs) BindContract(ctx context.Context, digest string) error {
	data, err := s.Get(ctx, digest)
	if err != nil {
		return err
	}
	c, err := DecodeContract(data, digest)
	if err != nil {
		return err
	}
	reads := append([]string{digest}, c.ContextDigests...)
	if c.KitDigest != "" {
		reads = append(reads, c.KitDigest)
	}
	for _, read := range reads {
		if err := s.AllowContractRead(ctx, digest, read); err != nil {
			return err
		}
	}
	return nil
}
func parentReadMarker(contractDigest, digest string) string {
	return "read-" + strings.TrimPrefix(contractDigest, "sha256:") + "-" + strings.TrimPrefix(digest, "sha256:")
}
