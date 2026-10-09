package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// holderSuffix names the sidecar next to a lock file that records who holds it.
// It is a separate file because Windows byte-range locks make the locked file
// itself unreadable to the waiters that need the record.
const holderSuffix = ".holder"

// Holder identifies the operation that announced itself as a lock's owner.
type Holder struct {
	Operation  string    `json:"operation"`
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

// Announce records operation as the owner of h so a waiter that gives up can
// name the holder through ReadHolder. Release removes the record before it
// unlocks, so the record never outlives the hold it describes, except when the
// holder crashes; the next Announce then overwrites it. It is diagnostics only:
// a failed write leaves the lock held and correct, just unattributed.
func (h *Handle) Announce(operation string) error {
	if h == nil {
		return errors.New("lock: announce on nil handle")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return errors.New("lock: announce on released handle")
	}
	data, err := json.Marshal(Holder{Operation: operation, PID: os.Getpid(), AcquiredAt: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("lock: encode holder: %w", err)
	}
	path := h.file.Name() + holderSuffix
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("lock: record holder %q: %w", path, err)
	}
	h.holderPath = path
	return nil
}

// ReadHolder returns the holder last announced for the lock at lockPath. It
// reports false when no holder announced itself or the record is unreadable,
// for example because it is being rewritten at that moment.
func ReadHolder(lockPath string) (Holder, bool) {
	data, err := os.ReadFile(lockPath + holderSuffix)
	if err != nil {
		return Holder{}, false
	}
	var holder Holder
	if err := json.Unmarshal(data, &holder); err != nil || holder.Operation == "" {
		return Holder{}, false
	}
	return holder, true
}
