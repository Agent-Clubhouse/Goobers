package decomposition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/providers"
)

// FileTargetLeaser shares target leases across local runner processes.
type FileTargetLeaser struct {
	Directory string
}

// Acquire waits interruptibly for the target's cross-process file lock.
func (l FileTargetLeaser) Acquire(ctx context.Context, repo providers.RepositoryRef, itemID string) (func() error, error) {
	if l.Directory == "" {
		return nil, fmt.Errorf("target lease directory is required")
	}
	if itemID == "" {
		return nil, fmt.Errorf("target item id is required")
	}
	if err := os.MkdirAll(l.Directory, 0o755); err != nil {
		return nil, fmt.Errorf("create target lease directory: %w", err)
	}
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", repo.Provider, repo.Owner, repo.Project, repo.Name, itemID)
	sum := sha256.Sum256([]byte(identity))
	path := filepath.Join(l.Directory, hex.EncodeToString(sum[:])+".lock")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		handle, err := lock.TryAcquire(path)
		if err == nil {
			return handle.Release, nil
		}
		if !errors.Is(err, lock.ErrHeld) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
