// Package instancelock manages daemon and manual instance lock state.
package instancelock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/lock"
)

// DaemonIdentity records the process and configuration pinned by a daemon lock.
type DaemonIdentity struct {
	PID                   int             `json:"pid"`
	StartedAt             time.Time       `json:"startedAt"`
	InstanceRoot          string          `json:"instanceRoot"`
	Version               string          `json:"version"`
	LivenessTimeoutMillis int64           `json:"livenessTimeoutMillis,omitempty"`
	Behavior              *DaemonBehavior `json:"behavior,omitempty"`
}

// DaemonBehavior records the runtime flags pinned by a daemon lock.
type DaemonBehavior struct {
	WatchConfig           bool  `json:"watchConfig"`
	Diagnostics           bool  `json:"diagnostics"`
	DrainTimeoutNanos     int64 `json:"drainTimeoutNanos"`
	SkipPreflight         bool  `json:"skipPreflight"`
	DisableReadModelReads bool  `json:"disableReadModelReads"`
	// MemoryHighWater, MemoryGateDisabled, and FsyncDisabled surface
	// GOOBERS_MEMORY_HIGH_WATER and GOOBERS_DISABLE_FSYNC (#4218), which
	// were previously visible only by reading the live daemon's own
	// environment. A separate `goobers status` invocation can only see this
	// process through up.lock, so the resolved values are published here at
	// startup rather than re-read from the environment by a process that
	// does not have it.
	MemoryHighWater    float64 `json:"memoryHighWater,omitempty"`
	MemoryGateDisabled bool    `json:"memoryGateDisabled,omitempty"`
	FsyncDisabled      bool    `json:"fsyncDisabled,omitempty"`
}

// HolderKind identifies whether a daemon or a manual run holds the lock.
type HolderKind string

// Supported lock holder kinds.
const (
	HolderDaemon HolderKind = "daemon"
	HolderManual HolderKind = "manual"
)

// State is the persisted lock holder record, including legacy daemon fields.
type State struct {
	PID                   int             `json:"pid,omitempty"`
	StartedAt             *time.Time      `json:"startedAt,omitempty"`
	InstanceRoot          string          `json:"instanceRoot,omitempty"`
	Version               string          `json:"version,omitempty"`
	LivenessTimeoutMillis int64           `json:"livenessTimeoutMillis,omitempty"`
	Behavior              *DaemonBehavior `json:"behavior,omitempty"`
	HolderKind            HolderKind      `json:"holderKind"`
	HolderPID             int             `json:"holderPid"`
}

// Acquire takes a non-blocking exclusive lock on lockPath so a
// second `goobers up` on the same instance root fails fast with a clear
// message (issue #23 AC3) instead of two daemons racing the same
// runs/scheduler state. The returned release func unlocks and closes the
// file; call it (typically via defer) when the holder exits.
func Acquire(lockPath string) (release func(), err error) {
	return AcquireWithIdentity(lockPath, nil)
}

// AcquireWithIdentity acquires the lock and persists the supplied daemon identity.
// A nil identity records a manual holder.
func AcquireWithIdentity(lockPath string, identity *DaemonIdentity) (release func(), err error) {
	held, err := lock.TryAcquire(lockPath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			state, _ := ReadStatePath(lockPath)
			if state != nil && state.HolderKind == HolderDaemon {
				return nil, fmt.Errorf(
					"another `goobers up` already holds the lock on this instance root (%s; holder pid %d)",
					lockPath,
					state.HolderPID,
				)
			}
			return nil, fmt.Errorf("another `goobers up` already holds the lock on this instance root (%s)", lockPath)
		}
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	f := held.File()
	if identity != nil {
		if err := instance.RequireCurrentRoot(identity.InstanceRoot); err != nil {
			return nil, errors.Join(err, held.Release())
		}
	}
	holderKind := HolderDaemon
	holderPID := os.Getpid()
	if identity == nil {
		holderKind = HolderManual
		state, _ := readInstanceLockState(f)
		if state != nil {
			identity, _ = state.DaemonIdentity()
		}
	} else {
		holderPID = identity.PID
	}
	if err := writeInstanceLockState(f, newInstanceLockState(identity, holderKind, holderPID)); err != nil {
		return nil, errors.Join(err, held.Release())
	}
	return func() {
		_ = held.Release()
	}, nil
}

func newInstanceLockState(identity *DaemonIdentity, holderKind HolderKind, holderPID int) State {
	state := State{
		HolderKind: holderKind,
		HolderPID:  holderPID,
	}
	if identity != nil {
		startedAt := identity.StartedAt
		state.PID = identity.PID
		state.StartedAt = &startedAt
		state.InstanceRoot = identity.InstanceRoot
		state.Version = identity.Version
		state.LivenessTimeoutMillis = identity.LivenessTimeoutMillis
		state.Behavior = identity.Behavior
	}
	return state
}

func writeInstanceLockState(f *os.File, state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode lock state: %w", err)
	}
	data = append(data, '\n')
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock state: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek lock state: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write lock state: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync lock state: %w", err)
	}
	return nil
}

// ReadDaemonIdentity reads and validates a daemon identity from a lock file.
func ReadDaemonIdentity(f *os.File) (*DaemonIdentity, error) {
	state, err := readInstanceLockState(f)
	if err != nil || state == nil {
		return nil, err
	}
	return state.DaemonIdentity()
}

func readInstanceLockState(f *os.File) (*State, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek lock state: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read lock state: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode lock state: %w", err)
	}
	identity, err := state.DaemonIdentity()
	if err != nil {
		return nil, err
	}
	switch state.HolderKind {
	case "":
		if identity == nil || state.HolderPID != 0 {
			return nil, errors.New("decode lock state: missing required field")
		}
	case HolderDaemon:
		if identity == nil || state.HolderPID != identity.PID {
			return nil, errors.New("decode lock state: daemon holder does not match identity")
		}
	case HolderManual:
		if state.HolderPID <= 0 {
			return nil, errors.New("decode lock state: missing manual holder pid")
		}
	default:
		return nil, fmt.Errorf("decode lock state: unknown holder kind %q", state.HolderKind)
	}
	return &state, nil
}

// ReadStatePath reads the persisted holder record at lockPath.
func ReadStatePath(lockPath string) (*State, error) {
	f, err := os.Open(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readInstanceLockState(f)
}

// DaemonIdentity validates and returns the daemon identity represented by s.
func (s State) DaemonIdentity() (*DaemonIdentity, error) {
	hasIdentity := s.PID != 0 || s.StartedAt != nil || s.InstanceRoot != "" || s.Version != ""
	if !hasIdentity {
		return nil, nil
	}
	if s.PID <= 0 || s.StartedAt == nil || s.StartedAt.IsZero() || s.InstanceRoot == "" || s.Version == "" {
		return nil, errors.New("decode daemon identity: missing required field")
	}
	return &DaemonIdentity{
		PID:                   s.PID,
		StartedAt:             *s.StartedAt,
		InstanceRoot:          s.InstanceRoot,
		Version:               s.Version,
		LivenessTimeoutMillis: s.LivenessTimeoutMillis,
		Behavior:              s.Behavior,
	}, nil
}

// InspectDaemonLock reports whether a daemon holds the lock and its identity.
func InspectDaemonLock(lockPath string) (running bool, identity *DaemonIdentity, err error) {
	f, err := os.Open(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("open lock file: %w", err)
	}
	_ = f.Close()

	held, err := lock.TryAcquire(lockPath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			state, readErr := ReadStatePath(lockPath)
			if readErr != nil || state == nil {
				return false, nil, readErr
			}
			identity, readErr := state.DaemonIdentity()
			if readErr != nil {
				return false, nil, readErr
			}
			return state.HolderKind == HolderDaemon, identity, nil
		}
		return false, nil, fmt.Errorf("inspect lock: %w", err)
	}
	defer func() { _ = held.Release() }()

	identity, err = ReadDaemonIdentity(held.File())
	return false, identity, err
}

// InspectDaemonLiveness evaluates the held daemon lock using its pinned timeout.
func InspectDaemonLiveness(lockPath string, now time.Time) (bool, *DaemonIdentity, daemonstate.Liveness, error) {
	running, identity, err := InspectDaemonLock(lockPath)
	if err != nil || !running {
		return running, identity, daemonstate.Liveness{}, err
	}
	lastTickAt, err := daemonstate.Read(lockPath)
	if err != nil {
		return false, identity, daemonstate.Liveness{}, err
	}
	timeout := instance.DefaultDaemonLivenessTimeout
	if identity != nil && identity.LivenessTimeoutMillis > 0 {
		timeout = time.Duration(identity.LivenessTimeoutMillis) * time.Millisecond
	}
	return true, identity, daemonstate.Evaluate(now, lastTickAt, timeout), nil
}
