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
	target := CleanupTarget{Path: path, WorktreeID: "pin-" + key, OwnerRunID: owner, Pinned: true}
	custody, custodyErr := readPinnedCustody(root)
	if custodyErr != nil && !os.IsNotExist(custodyErr) {
		return custodyErr
	}
	if custodyErr == nil {
		if owner != "" && owner != custody.OwnerRunID {
			return fmt.Errorf("pinned receipt owner differs from recovery custody")
		}
		target.OwnerRunID = custody.OwnerRunID
		target.Gaggle = custody.Gaggle
		target.RepositoryDigest = custody.RepositoryDigest
		target.CreatedAt = custody.CreatedAt
	}
	acknowledged, err := m.prepareCleanupTargetWithReceipts(ctx, target)
	if err != nil {
		return err
	}
	if !acknowledged {
		if _, err := os.Lstat(filepath.Join(path, "mutations.jsonl")); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		return fmt.Errorf("pinned mutation receipts require their durable handoff before reuse")
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
