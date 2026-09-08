package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// PublishSnapshotBundle durably publishes a private, immutable archive. Its
// parent must already exist outside any repository/worktree being cleaned up
// and be private to the instance. The coordinator still owns inventory bounds,
// metadata publication and cleanup authorization. Conflicting bytes are never
// replaced, and a failed attempt must never be treated as cleanup permission.
func PublishSnapshotBundle(ctx context.Context, repository, path string, record Record, maxBytes int64) (string, error) {
	return publishArchive(ctx, path, maxBytes, func(w io.Writer) error {
		_, err := WriteSnapshotBundle(ctx, repository, record, w, maxBytes)
		return err
	})
}

func publishArchive(ctx context.Context, path string, maxBytes int64, stream func(io.Writer) error) (string, error) {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return "", fmt.Errorf("invalid recovery archive byte budget")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	handle, err := platformlock.TryAcquire(path + ".lock")
	if err != nil {
		return "", fmt.Errorf("lock recovery archive: %w", err)
	}
	defer func() { _ = handle.Release() }()
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	digest := sha256.New()
	writer := &archiveBudgetWriter{destination: io.MultiWriter(file, digest), remaining: maxBytes}
	if err := stream(writer); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	want := fmt.Sprintf("sha256:%x", digest.Sum(nil))
	existing, err := archiveDigest(path, maxBytes)
	if err == nil && existing != want {
		return "", ErrRecordConflict
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Republish identical bytes on retries too: a prior rename may have
	// succeeded while its directory flush failed. Readability is not a flush.
	if err := durability.ReplaceFile(file.Name(), path); err != nil {
		return "", err
	}
	if err := durability.SyncDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	return want, nil
}

func archiveDigest(path string, maxBytes int64) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() > maxBytes {
		return "", fmt.Errorf("recovery archive is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return "", fmt.Errorf("recovery archive changed while opening")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxBytes {
		return "", fmt.Errorf("recovery archive exceeds byte budget")
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

type archiveBudgetWriter struct {
	destination io.Writer
	remaining   int64
}

func (w *archiveBudgetWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("recovery archive exceeds byte budget")
	}
	n, err := w.destination.Write(data)
	w.remaining -= int64(n)
	return n, err
}
