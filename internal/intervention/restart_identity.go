package intervention

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// restartEpochForCommand uses the canonical journal/trace ID shape for new
// epochs. A retained legacy identity always wins recovery; changing formatting
// must never execute a previously accepted human command a second time.
func restartEpochForCommand(runsDir, sourceRunID, scopedKey string) (string, error) {
	sum := sha256.Sum256([]byte(sourceRunID + "\x00" + scopedKey))
	canonical := hex.EncodeToString(sum[:16])
	legacy := "human-restart-" + hex.EncodeToString(sum[:])
	legacyExists, err := retainedRestartDirectory(filepath.Join(runsDir, legacy))
	if err != nil {
		return "", err
	}
	if !legacyExists {
		return canonical, nil
	}
	canonicalExists, err := retainedRestartDirectory(filepath.Join(runsDir, canonical))
	if err != nil {
		return "", err
	}
	if canonicalExists {
		return "", interventionConflict("restart_identity_conflict", "Both legacy and canonical continuations exist for this command; reconcile their custody before retrying.")
	}
	return legacy, nil
}
func retainedRestartDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, errors.New("restart identity path is not a retained journal directory")
	}
	return true, nil
}
