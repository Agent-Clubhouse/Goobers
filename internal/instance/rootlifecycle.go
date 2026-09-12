package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// RootDecommissionFileName stores the identity-bound historical-root marker.
const RootDecommissionFileName = ".instance-decommissioned"
const maxRootDecommissionBytes = 4096

// ErrHistoricalRoot indicates an explicitly decommissioned instance root.
var ErrHistoricalRoot = errors.New("historical root; do not use")

// RequireCurrentRoot refuses historical or unverifiable lifecycle metadata.
// Missing markers are compatible with legacy roots; this call never adopts one.
func RequireCurrentRoot(root string) error {
	marker, err := ReadRootDecommission(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("root lifecycle could not be verified: %w", err)
	}
	return fmt.Errorf("%w: decommissioned %s, reason %q", ErrHistoricalRoot, marker.At.Format(time.RFC3339Nano), marker.Reason)
}

// RootDecommission is a singleton historical-root marker, bound to its durable
// ID. Its four-line versioned encoding is bounded and never an append-only log.
type RootDecommission struct {
	InstanceID string
	At         time.Time
	Reason     string
}

// ReadRootDecommission reads and validates a marker without changing the root.
func ReadRootDecommission(root string) (*RootDecommission, error) {
	path := filepath.Join(root, RootDecommissionFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("decommission marker is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("decommission marker changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRootDecommissionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRootDecommissionBytes {
		return nil, fmt.Errorf("decommission marker exceeds bound")
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) != 5 || parts[0] != "goobers-root-decommission-v1" || parts[4] != "" {
		return nil, fmt.Errorf("invalid decommission marker")
	}
	id, err := ReadRootIdentity(root)
	if err != nil {
		// Only a missing marker means not decommissioned. A missing identity
		// beside an existing marker must remain an invalid historical state.
		return nil, errors.New("decommission marker identity is unavailable: " + err.Error())
	}
	if parts[1] != id {
		return nil, fmt.Errorf("decommission marker belongs to a different instance ID")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[2])
	if err != nil || at.IsZero() {
		return nil, fmt.Errorf("invalid decommission timestamp")
	}
	reason, err := strconv.Unquote(parts[3])
	if err != nil || strings.TrimSpace(reason) == "" || len(reason) > 2048 {
		return nil, fmt.Errorf("invalid decommission reason")
	}
	return &RootDecommission{InstanceID: id, At: at, Reason: reason}, nil
}

// DecommissionRoot is called only while the operator holds the instance's
// maintenance/daemon-exclusion lock. It never deletes journals or other data.
func DecommissionRoot(ctx context.Context, root, reason string, at time.Time) (*RootDecommission, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 2048 || at.IsZero() {
		return nil, fmt.Errorf("decommission requires a bounded reason and timestamp")
	}
	id, err := EnsureRootIdentity(ctx, root)
	if err != nil {
		return nil, err
	}
	handle, err := acquireRootIdentityLock(ctx, filepath.Join(root, RootIdentityFileName+".lock"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()
	if existing, err := ReadRootDecommission(root); err == nil || !errors.Is(err, os.ErrNotExist) {
		return existing, err
	}
	data := []byte("goobers-root-decommission-v1\n" + id + "\n" + at.UTC().Format(time.RFC3339Nano) + "\n" + strconv.Quote(reason) + "\n")
	if len(data) > maxRootDecommissionBytes {
		return nil, fmt.Errorf("encoded decommission marker exceeds bound")
	}
	if err := journal.WriteFileAtomic(filepath.Join(root, RootDecommissionFileName), data, 0o644); err != nil {
		return nil, err
	}
	return &RootDecommission{InstanceID: id, At: at.UTC(), Reason: reason}, nil
}
