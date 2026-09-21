package configgeneration

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// MaxGenerations is the default upper bound on retained execution generations.
const MaxGenerations = 64

// MaxStoredBytes bounds archives, their shared-store copy, and extracted trees.
const MaxStoredBytes = int64(256 << 20)

// Store retains extracted immutable generations and publishes their archives
// through the existing content-addressed stage transport. Workers use a local
// cache; shared leases protect concurrent readers from catalogue pruning.
type Store struct {
	Root  string
	Blobs *blobstore.Dir
	// LocalCache retains fetched archives without publishing or deleting shared blobs.
	LocalCache bool
	// DurablePins is re-read only after exclusive ownership excludes live readers.
	DurablePins func(context.Context) (map[string]bool, error)
}

// Keep captures a generation before run admission. Protected includes all
// retained journal pins and the currently-applied generation, including paused
// and recoverable runs. Capacity exhaustion refuses new admission; it never
// evicts an authoritative pin.
func (s Store) Keep(ctx context.Context, data []byte, digest string, protected map[string]bool) (string, error) {
	path, held, err := s.keep(ctx, data, digest, protected, false, false)
	if held != nil {
		_ = held.Release()
	}
	return path, err
}

// KeepAndAcquire publishes a generation and acquires a shared use lease in one
// store transaction. Pruning cannot interleave before the caller owns its lease.
func (s Store) KeepAndAcquire(ctx context.Context, data []byte, digest string, protected map[string]bool) (string, *lock.Handle, error) {
	return s.keep(ctx, data, digest, protected, true, false)
}

// KeepExternallyOwned retains a standalone engine generation before its
// external history can reference it. No local journal-retention signal exists
// for this owner, so automatic pruning must not remove it. The same finite
// generation and byte limits apply; capacity exhaustion refuses admission.
func (s Store) KeepExternallyOwned(ctx context.Context, data []byte, digest string) (string, *lock.Handle, error) {
	return s.keep(ctx, data, digest, nil, true, true)
}

func (s Store) keep(ctx context.Context, data []byte, digest string, protected map[string]bool, acquireLease, externalOwner bool) (string, *lock.Handle, error) {
	archive, err := Decode(data, digest)
	if err != nil {
		return "", nil, err
	}
	if s.Blobs == nil && !s.LocalCache {
		return "", nil, errors.New("config generation has no durable blob store")
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return "", nil, err
	}
	held, err := s.lock(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = held.Release() }()
	directory, err := s.keepLocked(ctx, archive, data, digest, protected)
	if err != nil {
		return "", nil, err
	}
	if externalOwner {
		marker := filepath.Join(filepath.Dir(directory), "external-owner")
		if _, err := os.Lstat(marker); errors.Is(err, fs.ErrNotExist) {
			if err := writeArchiveFile(marker, []byte("external-history\n")); err != nil {
				return "", nil, err
			}
			if err := durability.SyncDir(filepath.Dir(directory)); err != nil {
				return "", nil, err
			}
		} else if err != nil {
			return "", nil, err
		}
	}
	if !acquireLease {
		return directory, nil, nil
	}
	lease, err := lock.TryAcquireShared(filepath.Join(filepath.Dir(directory), "lease.lock"))
	if err != nil {
		return "", nil, err
	}
	return directory, lease, nil
}

func (s Store) keepLocked(ctx context.Context, archive *Archive, data []byte, digest string, protected map[string]bool) (string, error) {
	destination := filepath.Join(s.Root, strings.TrimPrefix(digest, "sha256:"))
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("config generation cache is not a directory")
		}
		if _, err := s.Load(ctx, digest); err != nil {
			return "", err
		}
		if err := s.publish(ctx, digest, data); err != nil {
			return "", err
		}
		return filepath.Abs(filepath.Join(destination, "config"))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	needed := int64(len(data)) + 32
	if s.Blobs != nil {
		needed += int64(len(data))
	}
	for _, f := range archive.Files {
		needed += int64(len(f.Data))
	}
	if err := s.prune(ctx, protected, needed); err != nil {
		return "", err
	}
	staged, err := os.MkdirTemp(s.Root, ".pending-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := archive.Extract(ctx, staged); err != nil {
		return "", err
	}
	if err := writeArchiveFile(filepath.Join(staged, "archive.json"), data); err != nil {
		return "", err
	}
	if err := durability.Move(staged, destination); err != nil {
		return "", err
	}
	if err := durability.SyncDir(s.Root); err != nil {
		return "", err
	}
	// Catalogue first: a failed publication remains discoverable and bounded,
	// rather than orphaning an archive in the shared blob store.
	if err := s.publish(ctx, digest, data); err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(destination, "config"))
}

// Load verifies the retained execution tree before returning it. A missing or
// corrupt retained generation refuses resume; it never falls back to live config.
func (s Store) Load(ctx context.Context, digest string) (string, error) {
	name := strings.TrimPrefix(digest, "sha256:")
	decoded, err := hex.DecodeString(name)
	if err != nil || len(decoded) != 32 || digest != "sha256:"+name || strings.ToLower(name) != name {
		return "", errors.New("invalid config generation digest")
	}
	directory := filepath.Join(s.Root, name)
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("config generation cache is not a directory")
	}
	configDir := filepath.Join(directory, "config")
	archive, err := readRetainedArchive(directory, digest)
	if err != nil {
		return "", err
	}
	_, actual, err := CaptureForInstance(ctx, configDir, archive.InstanceID)
	if err != nil {
		return "", err
	}
	if actual != digest {
		return "", errors.New("retained config generation is corrupt")
	}
	return filepath.Abs(configDir)
}

func (s Store) prune(ctx context.Context, protected map[string]bool, needed int64) error {
	candidates, total, err := s.pruneCandidates(ctx)
	if err != nil {
		return err
	}
	count := len(candidates)
	for _, c := range candidates {
		if count < MaxGenerations && total+needed <= MaxStoredBytes {
			return nil
		}
		if protected[c.digest] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(s.Root, c.name, "external-owner")); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		lease, err := lock.TryAcquire(filepath.Join(s.Root, c.name, "lease.lock"))
		if errors.Is(err, lock.ErrHeld) {
			continue
		}
		if err != nil {
			return err
		}
		if s.DurablePins != nil {
			pins, readErr := s.DurablePins(ctx)
			if readErr != nil {
				_ = lease.Release()
				return readErr
			}
			if pins[c.digest] {
				_ = lease.Release()
				continue
			}
		}
		// Store lock still excludes new readers after releasing the lease;
		// release before removal so Windows can delete the directory too.
		if err := lease.Release(); err != nil {
			return err
		}
		if s.Blobs != nil {
			if err := s.Blobs.Remove(ctx, c.digest); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(filepath.Join(s.Root, c.name)); err != nil {
			return err
		}
		count--
		total -= c.size
	}
	if count >= MaxGenerations || total+needed > MaxStoredBytes {
		return errors.New("config generation retention capacity exhausted by protected runs")
	}
	return nil
}

func (s Store) publish(ctx context.Context, digest string, data []byte) error {
	if s.Blobs == nil {
		return nil
	}
	return s.Blobs.Put(ctx, digest, data)
}

// Acquire verifies and leases an existing generation for recovery.
func (s Store) Acquire(ctx context.Context, digest string) (string, *lock.Handle, error) {
	held, err := s.lock(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = held.Release() }()
	directory, err := s.Load(ctx, digest)
	if err != nil {
		return "", nil, err
	}
	lease, err := lock.TryAcquireShared(filepath.Join(filepath.Dir(directory), "lease.lock"))
	if err != nil {
		return "", nil, err
	}
	return directory, lease, nil
}

func readRetainedArchive(directory, digest string) (*Archive, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := safeopen.OpenRegularInRoot(root, "archive.json")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxArchiveBytes {
		return nil, errors.New("invalid retained config archive")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	return Decode(data, digest)
}

func (s Store) lock(ctx context.Context) (*lock.Handle, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		held, err := lock.TryAcquire(filepath.Join(s.Root, "store.lock"))
		if !errors.Is(err, lock.ErrHeld) {
			return held, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func writeArchiveFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

type generationCandidate struct {
	name, digest string
	size         int64
}

func (s Store) pruneCandidates(ctx context.Context) ([]generationCandidate, int64, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, 0, err
	}
	var candidates []generationCandidate
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if strings.HasPrefix(entry.Name(), ".pending-") {
			if err := os.RemoveAll(filepath.Join(s.Root, entry.Name())); err != nil {
				return nil, 0, err
			}
			continue
		}
		if !entry.IsDir() {
			continue
		}
		digest := "sha256:" + entry.Name()
		if len(entry.Name()) != 64 {
			return nil, 0, fmt.Errorf("unexpected config generation entry %q", entry.Name())
		}
		var size int64
		err := filepath.WalkDir(filepath.Join(s.Root, entry.Name()), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("config generation contains symlink")
			}
			if !d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				size += info.Size()
				if d.Name() == "archive.json" && s.Blobs != nil {
					size += info.Size()
				}
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		candidates = append(candidates, generationCandidate{entry.Name(), digest, size})
		total += size
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })

	return candidates, total, nil
}
