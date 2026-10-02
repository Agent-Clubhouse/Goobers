package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// MaxSurrenderReadBytes matches the daemon's existing surrender write budget.
const MaxSurrenderReadBytes = 1 << 20

// GetBounded serves network reads without unbounded allocation or following a
// substituted non-regular file. The identity hash remains rooted in this plane.
func (d *SurrenderDir) GetBounded(ctx context.Context, run, stage string, attempt int, limit int64) ([]byte, error) {
	if limit <= 0 || limit > MaxSurrenderReadBytes {
		return nil, errors.New("dispatcher: invalid surrender read budget")
	}
	path, err := d.path(run, stage, attempt)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(d.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := safeopen.OpenRegularInRoot(root, filepath.Base(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoSurrender
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("dispatcher: surrendered result exceeds %d byte budget", limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
