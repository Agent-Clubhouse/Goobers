package localscheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type ownedJSONState interface {
	setOwnershipStamp(ownershipStamp)
}

func writeOwnedJSONState(
	schedulerDir string,
	owner *stateOwner,
	fileName string,
	noun string,
	directoryNoun string,
	state ownedJSONState,
	writeFile func(string, []byte, os.FileMode) error,
) error {
	stamp, err := owner.stamp(schedulerDir, fileName)
	if err != nil {
		return err
	}
	state.setOwnershipStamp(stamp)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("localscheduler: marshal %s: %w", noun, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return fmt.Errorf("localscheduler: create %s directory: %w", directoryNoun, err)
	}
	if err := writeFile(filepath.Join(schedulerDir, fileName), data, 0o644); err != nil {
		return fmt.Errorf("localscheduler: persist %s: %w", noun, err)
	}
	// A failed write must remain retryable instead of poisoning later writes
	// with ErrStateSeized.
	owner.commit(fileName, stamp)
	return nil
}
