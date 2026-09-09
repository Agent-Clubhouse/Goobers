package blobstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// BoundedReader is an optional store capability for evidence consumers that
// must enforce a byte limit before fetching or allocating the body. Callers
// must not silently fall back to the unbounded Store.Get operation.
type BoundedReader interface {
	GetBounded(context.Context, string, int64) ([]byte, error)
}

// ErrTooLarge distinguishes a refused payload from a transient store failure.
var ErrTooLarge = errors.New("blobstore: blob exceeds read limit")

// GetBounded reads at most limit bytes with descriptor-relative containment and
// regular-file checks. The returned content still must match its address.
func (d *Dir) GetBounded(ctx context.Context, digest string, limit int64) (data []byte, retErr error) {
	if limit <= 0 || limit > 64<<20 {
		return nil, ErrTooLarge
	}
	full, err := d.pathFor(digest)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(d.Root, full)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(d.Root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := root.Close(); err != nil {
			data = nil
			retErr = errors.Join(retErr, err)
		}
	}()
	file, err := safeopen.OpenRegularInRoot(root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, safeopen.ErrNotRegular) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			data = nil
			retErr = errors.Join(retErr, err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, ErrTooLarge
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
		return nil, ErrNotFound
	}
	return data, nil
}
