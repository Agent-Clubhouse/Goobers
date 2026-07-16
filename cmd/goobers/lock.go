package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/version"
)

type daemonIdentity struct {
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"startedAt"`
	InstanceRoot string    `json:"instanceRoot"`
	Version      string    `json:"version"`
}

// acquireInstanceLock takes a non-blocking exclusive flock on lockPath so a
// second `goobers up` on the same instance root fails fast with a clear
// message (issue #23 AC3) instead of two daemons racing the same
// runs/scheduler state. The returned release func unlocks and closes the
// file; call it (typically via defer) when the holder exits.
func acquireInstanceLock(lockPath string) (release func(), err error) {
	return acquireInstanceLockWithIdentity(lockPath, nil)
}

func acquireDaemonLock(lockPath, instanceRoot string) (release func(), err error) {
	absoluteRoot, err := filepath.Abs(instanceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve instance root: %w", err)
	}
	identity := daemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		InstanceRoot: absoluteRoot,
		Version:      version.Get().String(),
	}
	return acquireInstanceLockWithIdentity(lockPath, &identity)
}

func acquireInstanceLockWithIdentity(lockPath string, identity *daemonIdentity) (release func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holder, _ := readDaemonIdentity(f)
			_ = f.Close()
			holderRunning := false
			if holder != nil {
				holderRunning, _ = processExists(holder.PID)
			}
			if holderRunning {
				return nil, fmt.Errorf(
					"another `goobers up` already holds the lock on this instance root (%s; holder pid %d)",
					lockPath,
					holder.PID,
				)
			}
			return nil, fmt.Errorf("another `goobers up` already holds the lock on this instance root (%s)", lockPath)
		}
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if identity != nil {
		if err := writeDaemonIdentity(f, *identity); err != nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return nil, err
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func writeDaemonIdentity(f *os.File, identity daemonIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode daemon identity: %w", err)
	}
	data = append(data, '\n')
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate daemon identity: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek daemon identity: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write daemon identity: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync daemon identity: %w", err)
	}
	return nil
}

func readDaemonIdentity(f *os.File) (*daemonIdentity, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek daemon identity: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read daemon identity: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var identity daemonIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return nil, fmt.Errorf("decode daemon identity: %w", err)
	}
	if identity.PID <= 0 || identity.StartedAt.IsZero() || identity.InstanceRoot == "" || identity.Version == "" {
		return nil, errors.New("decode daemon identity: missing required field")
	}
	return &identity, nil
}

func inspectDaemonLock(lockPath string) (running bool, identity *daemonIdentity, err error) {
	f, err := os.Open(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			identity, readErr := readDaemonIdentity(f)
			if readErr != nil || identity == nil {
				return false, nil, readErr
			}
			running, processErr := processExists(identity.PID)
			return running, identity, processErr
		}
		return false, nil, fmt.Errorf("inspect lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	identity, err = readDaemonIdentity(f)
	return false, identity, err
}

func processExists(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("inspect daemon pid %d: %w", pid, err)
	}
}
