package runtimewiring

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/worktree"
)

// RecoveryCleanupBaseRef selects the configured base reference for recovery capture.
func RecoveryCleanupBaseRef(target worktree.CleanupTarget) (string, error) {
	baseRef := strings.TrimSpace(target.BaseRef)
	if baseRef == "" {
		return "", fmt.Errorf("recovery cleanup requires the owning run's base reference")
	}
	return baseRef, nil
}

// PrepareRecoveryInventory opens or initializes the durable recovery inventory.
func PrepareRecoveryInventory(instanceRoot string) (string, error) {
	root := filepath.Join(instanceRoot, "recovery")
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("recovery inventory must be a real directory")
	}
	if err := durability.SyncDir(instanceRoot); err != nil {
		return "", err
	}
	return root, nil
}

// RecoveryCleanupJournal appends scrubbed cleanup events to a run journal.
type RecoveryCleanupJournal struct {
	Directory string
	Scrubber  journal.Scrubber
}

// Append appends a scrubbed event to the recovery journal.
func (l RecoveryCleanupJournal) Append(event journal.Event) error {
	log, _, err := journal.OpenInstanceLog(l.Directory, journal.WithScrubber(l.Scrubber))
	if err != nil {
		return err
	}
	return errors.Join(log.Append(event), log.Close())
}
