package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

const pinnedReceiptOwnerFile = "pin.receipt-owner"

// This owner survives lease release. It is replaced only after the preceding
// run's sidecar has been handed off and removed, never before destructive git
// preparation. One record per pinned repository bounds its lifetime/storage.
func (m *Manager) handoffPinnedReceipts(ctx context.Context, key string) error {
	root := filepath.Join(m.pinnedRoot, key)
	path := filepath.Join(root, "pin")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	owner, err := readPinnedReceiptOwner(root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := m.prepareCleanup(ctx, path, "pin-"+key, owner); err != nil {
		return err
	}
	if m.beforeCleanup == nil {
		return nil
	}
	// Do not carry a prior run's receipts into the next run's sidecar. A
	// failed remove/sync preserves the old owner and refuses preparation.
	if err := os.Remove(filepath.Join(path, "mutations.jsonl")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return durability.SyncDir(path)
}

func readPinnedReceiptOwner(root string) (string, error) {
	dir, err := safeopen.Open(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = dir.Close() }()
	f, err := safeopen.OpenAt(dir, pinnedReceiptOwnerFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 256 {
		return "", fmt.Errorf("invalid pinned receipt owner record")
	}
	data, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", err
	}
	if len(data) > 256 || !validRunID(string(data)) {
		return "", fmt.Errorf("invalid pinned receipt owner")
	}
	return string(data), nil
}
