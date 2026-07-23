package daemonstate

import (
	"fmt"
	"os"
	"time"
)

// Liveness is the scheduler heartbeat evaluated at a point in time.
type Liveness struct {
	LastTickAt time.Time
	Age        time.Duration
	Timeout    time.Duration
	Healthy    bool
}

// Refresh records a completed scheduler tick in the lock timestamp, avoiding
// replacement of the inode that carries the process lock.
func Refresh(lockPath string, tickAt time.Time) error {
	if tickAt.IsZero() {
		return fmt.Errorf("refresh daemon heartbeat: tick time is required")
	}
	if err := os.Chtimes(lockPath, tickAt, tickAt); err != nil {
		return fmt.Errorf("refresh daemon heartbeat: %w", err)
	}
	return nil
}

// Read returns the most recent scheduler heartbeat recorded on the daemon lock.
func Read(lockPath string) (time.Time, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("read daemon heartbeat: %w", err)
	}
	return info.ModTime().UTC(), nil
}

// Evaluate applies the configured freshness threshold to a heartbeat.
func Evaluate(now, lastTickAt time.Time, timeout time.Duration) Liveness {
	age := now.Sub(lastTickAt)
	if age < 0 {
		age = 0
	}
	return Liveness{
		LastTickAt: lastTickAt.UTC(),
		Age:        age,
		Timeout:    timeout,
		Healthy:    !lastTickAt.IsZero() && timeout > 0 && age <= timeout,
	}
}
