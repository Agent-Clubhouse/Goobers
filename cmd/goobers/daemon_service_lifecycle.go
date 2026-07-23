package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

const dirtyRestartCause = "previous daemon lock remained without a clean-shutdown event"

type priorDaemonLock struct {
	exists   bool
	identity *daemonIdentity
	readErr  string
}

func readPriorDaemonLock(path string) (priorDaemonLock, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return priorDaemonLock{}, nil
	} else if err != nil {
		return priorDaemonLock{}, fmt.Errorf("inspect previous daemon lock: %w", err)
	}
	state, err := readInstanceLockStatePath(path)
	if err != nil {
		return priorDaemonLock{exists: true, readErr: err.Error()}, nil
	}
	var identity *daemonIdentity
	if state != nil {
		identity, err = state.daemonIdentity()
		if err != nil {
			return priorDaemonLock{}, fmt.Errorf("read previous daemon identity: %w", err)
		}
	}
	return priorDaemonLock{exists: true, identity: identity}, nil
}

func readCurrentDaemonIdentity(path string) (*daemonIdentity, error) {
	state, err := readInstanceLockStatePath(path)
	if err != nil {
		return nil, err
	}
	if state == nil || state.HolderKind != lockHolderDaemon {
		return nil, errors.New("daemon lock does not contain the current daemon identity")
	}
	identity, err := state.daemonIdentity()
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, errors.New("daemon lock does not contain the current daemon identity")
	}
	return identity, nil
}

func journalDaemonStart(log *journal.InstanceLog, prior priorDaemonLock, current *daemonIdentity) error {
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		return fmt.Errorf("read daemon lifecycle journal: %w", err)
	}
	hasPriorDaemon := prior.identity != nil || prior.readErr != ""
	if prior.exists && hasPriorDaemon && !lastDaemonShutdownWasClean(events, prior.identity) {
		payload := daemonLifecyclePayload(prior.identity)
		if payload == nil {
			payload = make(map[string]any)
		}
		if prior.readErr != "" {
			payload["lockReadError"] = prior.readErr
		}
		event := journal.Event{
			Type:   journal.EventDaemonDirtyRestart,
			Reason: dirtyRestartCause,
			Runner: payload,
		}
		if err := log.Append(event); err != nil {
			return fmt.Errorf("journal dirty daemon restart: %w", err)
		}
	}
	if err := log.Append(journal.Event{
		Type:   journal.EventDaemonStarted,
		Runner: daemonLifecyclePayload(current),
	}); err != nil {
		return fmt.Errorf("journal daemon start: %w", err)
	}
	return nil
}

func journalDaemonCleanShutdown(log *journal.InstanceLog, current *daemonIdentity) error {
	if err := log.Append(journal.Event{
		Type:   journal.EventDaemonCleanShutdown,
		Reason: "graceful shutdown completed",
		Runner: daemonLifecyclePayload(current),
	}); err != nil {
		return fmt.Errorf("journal clean daemon shutdown: %w", err)
	}
	return nil
}

func lastDaemonShutdownWasClean(events []journal.Event, identity *daemonIdentity) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventDaemonCleanShutdown:
			return daemonLifecycleEventMatches(events[i], identity)
		case journal.EventDaemonStarted, journal.EventDaemonDirtyRestart:
			return false
		}
	}
	return false
}

func daemonLifecycleEventMatches(event journal.Event, identity *daemonIdentity) bool {
	if identity == nil || event.Runner == nil {
		return false
	}
	pid, ok := event.Runner["pid"].(float64)
	if !ok || int(pid) != identity.PID {
		return false
	}
	startedAt, ok := event.Runner["startedAt"].(string)
	if !ok {
		return false
	}
	return startedAt == identity.StartedAt.UTC().Format(time.RFC3339Nano)
}

func daemonLifecyclePayload(identity *daemonIdentity) map[string]any {
	if identity == nil {
		return nil
	}
	return map[string]any{
		"pid":          identity.PID,
		"startedAt":    identity.StartedAt.UTC().Format(time.RFC3339Nano),
		"instanceRoot": identity.InstanceRoot,
		"version":      identity.Version,
	}
}
