package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/version"
)

type daemonIdentity struct {
	PID                   int       `json:"pid"`
	StartedAt             time.Time `json:"startedAt"`
	InstanceRoot          string    `json:"instanceRoot"`
	Version               string    `json:"version"`
	LivenessTimeoutMillis int64     `json:"livenessTimeoutMillis,omitempty"`
}

type lockHolderKind string

const (
	lockHolderDaemon lockHolderKind = "daemon"
	lockHolderManual lockHolderKind = "manual"
)

type instanceLockState struct {
	PID                   int            `json:"pid,omitempty"`
	StartedAt             *time.Time     `json:"startedAt,omitempty"`
	InstanceRoot          string         `json:"instanceRoot,omitempty"`
	Version               string         `json:"version,omitempty"`
	LivenessTimeoutMillis int64          `json:"livenessTimeoutMillis,omitempty"`
	HolderKind            lockHolderKind `json:"holderKind"`
	HolderPID             int            `json:"holderPid"`
}

// acquireInstanceLock takes a non-blocking exclusive lock on lockPath so a
// second `goobers up` on the same instance root fails fast with a clear
// message (issue #23 AC3) instead of two daemons racing the same
// runs/scheduler state. The returned release func unlocks and closes the
// file; call it (typically via defer) when the holder exits.
func acquireInstanceLock(lockPath string) (release func(), err error) {
	return acquireInstanceLockWithIdentity(lockPath, nil)
}

func acquireDaemonLock(lockPath, instanceRoot string) (release func(), err error) {
	return acquireDaemonLockWithTimeout(lockPath, instanceRoot, instance.DefaultDaemonLivenessTimeout)
}

func acquireDaemonLockWithTimeout(lockPath, instanceRoot string, livenessTimeout time.Duration) (release func(), err error) {
	if livenessTimeout <= 0 {
		return nil, fmt.Errorf("daemon liveness timeout must be positive")
	}
	absoluteRoot, err := filepath.Abs(instanceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve instance root: %w", err)
	}
	identity := daemonIdentity{
		PID:                   os.Getpid(),
		StartedAt:             time.Now().UTC(),
		InstanceRoot:          absoluteRoot,
		Version:               version.Get().String(),
		LivenessTimeoutMillis: livenessTimeout.Milliseconds(),
	}
	return acquireInstanceLockWithIdentity(lockPath, &identity)
}

func acquireInstanceLockWithIdentity(lockPath string, identity *daemonIdentity) (release func(), err error) {
	held, err := lock.TryAcquire(lockPath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			state, _ := readInstanceLockStatePath(lockPath)
			if state != nil && state.HolderKind == lockHolderDaemon {
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
	holderKind := lockHolderDaemon
	holderPID := os.Getpid()
	if identity == nil {
		holderKind = lockHolderManual
		state, _ := readInstanceLockState(f)
		if state != nil {
			identity, _ = state.daemonIdentity()
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

func newInstanceLockState(identity *daemonIdentity, holderKind lockHolderKind, holderPID int) instanceLockState {
	state := instanceLockState{
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
	}
	return state
}

func writeInstanceLockState(f *os.File, state instanceLockState) error {
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

func readDaemonIdentity(f *os.File) (*daemonIdentity, error) {
	state, err := readInstanceLockState(f)
	if err != nil || state == nil {
		return nil, err
	}
	return state.daemonIdentity()
}

func readInstanceLockState(f *os.File) (*instanceLockState, error) {
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
	var state instanceLockState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode lock state: %w", err)
	}
	identity, err := state.daemonIdentity()
	if err != nil {
		return nil, err
	}
	switch state.HolderKind {
	case "":
		if identity == nil || state.HolderPID != 0 {
			return nil, errors.New("decode lock state: missing required field")
		}
	case lockHolderDaemon:
		if identity == nil || state.HolderPID != identity.PID {
			return nil, errors.New("decode lock state: daemon holder does not match identity")
		}
	case lockHolderManual:
		if state.HolderPID <= 0 {
			return nil, errors.New("decode lock state: missing manual holder pid")
		}
	default:
		return nil, fmt.Errorf("decode lock state: unknown holder kind %q", state.HolderKind)
	}
	return &state, nil
}

func readInstanceLockStatePath(lockPath string) (*instanceLockState, error) {
	f, err := os.Open(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readInstanceLockState(f)
}

func (s instanceLockState) daemonIdentity() (*daemonIdentity, error) {
	hasIdentity := s.PID != 0 || s.StartedAt != nil || s.InstanceRoot != "" || s.Version != ""
	if !hasIdentity {
		return nil, nil
	}
	if s.PID <= 0 || s.StartedAt == nil || s.StartedAt.IsZero() || s.InstanceRoot == "" || s.Version == "" {
		return nil, errors.New("decode daemon identity: missing required field")
	}
	return &daemonIdentity{
		PID:                   s.PID,
		StartedAt:             *s.StartedAt,
		InstanceRoot:          s.InstanceRoot,
		Version:               s.Version,
		LivenessTimeoutMillis: s.LivenessTimeoutMillis,
	}, nil
}

func inspectDaemonLock(lockPath string) (running bool, identity *daemonIdentity, err error) {
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
			state, readErr := readInstanceLockStatePath(lockPath)
			if readErr != nil || state == nil {
				return false, nil, readErr
			}
			identity, readErr := state.daemonIdentity()
			if readErr != nil {
				return false, nil, readErr
			}
			return state.HolderKind == lockHolderDaemon, identity, nil
		}
		return false, nil, fmt.Errorf("inspect lock: %w", err)
	}
	defer func() { _ = held.Release() }()

	identity, err = readDaemonIdentity(held.File())
	return false, identity, err
}

func inspectDaemonLiveness(lockPath string, now time.Time) (bool, *daemonIdentity, daemonstate.Liveness, error) {
	running, identity, err := inspectDaemonLock(lockPath)
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
