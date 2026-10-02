package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instancelock"
	"github.com/goobers/goobers/internal/version"
)

type daemonIdentity = instancelock.DaemonIdentity
type daemonBehavior = instancelock.DaemonBehavior
type instanceLockState = instancelock.State

const (
	lockHolderDaemon = instancelock.HolderDaemon
	lockHolderManual = instancelock.HolderManual
)

func acquireInstanceLock(lockPath string) (release func(), err error) {
	return instancelock.Acquire(lockPath)
}

func acquireDaemonLock(
	lockPath, instanceRoot string,
	livenessTimeout time.Duration,
	behavior *daemonBehavior,
) (release func(), err error) {
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
		Behavior:              behavior,
	}
	return acquireInstanceLockWithIdentity(lockPath, &identity)
}

func acquireInstanceLockWithIdentity(lockPath string, identity *daemonIdentity) (release func(), err error) {
	return instancelock.AcquireWithIdentity(lockPath, identity)
}

func readInstanceLockStatePath(lockPath string) (*instanceLockState, error) {
	return instancelock.ReadStatePath(lockPath)
}

func inspectDaemonLock(lockPath string) (running bool, identity *daemonIdentity, err error) {
	return instancelock.InspectDaemonLock(lockPath)
}

func inspectDaemonLiveness(lockPath string, now time.Time) (bool, *daemonIdentity, daemonstate.Liveness, error) {
	return instancelock.InspectDaemonLiveness(lockPath, now)
}
