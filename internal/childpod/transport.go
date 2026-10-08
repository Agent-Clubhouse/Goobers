package childpod

import (
	"bytes"
	"context"
	"os"
	"time"

	"github.com/goobers/goobers/internal/recovery"
)

// CaptureCarrier snapshots committed and permitted dirty files under exclusive
// custody and emits no ancestor history. identityTime is stable across replay.
func CaptureCarrier(ctx context.Context, path, repositoryKey, runID string, at time.Time, policy recovery.SnapshotPolicy) (Carrier, recovery.ChildSnapshot, error) {
	snapshot, err := recovery.CaptureChildSnapshot(ctx, path, repositoryKey, runID, at, at.Add(30*24*time.Hour), policy)
	if err != nil {
		return Carrier{}, snapshot, err
	}
	var data bytes.Buffer
	portable, err := recovery.WritePortableSnapshot(ctx, path, snapshot, &data, MaxBundleBytes)
	if err != nil {
		return Carrier{}, snapshot, err
	}
	return Carrier{Snapshot: portable, Bundle: data.Bytes()}, snapshot, nil
}

func withBundle(c Carrier, call func(string) error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	f, err := os.CreateTemp("", "goobers-child-carrier-*.bundle")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(c.Bundle); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return call(f.Name())
}

// Materialize initializes a fresh private volume; it never fetches a remote.
func Materialize(ctx context.Context, path string, c Carrier) error {
	return withBundle(c, func(archive string) error {
		return recovery.InitializePortableWorkspace(ctx, path, archive, c.Snapshot, MaxBundleBytes)
	})
}
