//go:build linux

package childpod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// VerifyEntrypoint requires the dispatcher-owned private PID namespace.
func VerifyEntrypoint() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("isolated child supervisor must be PID 1 in its private Linux container")
	}
	return nil
}

// Quiesce stops and reaps every process in this private namespace, including
// double-forked detached writers. No snapshot follows a refusal or timeout.
func Quiesce(ctx context.Context) error {
	if err := VerifyEntrypoint(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Linux excludes the caller and PID1 from kill(-1). Non-root execution,
		// dropped capabilities and no shared PID namespace are host-verified.
		if err := unix.Kill(-1, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
		for {
			pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
			if errors.Is(err, unix.ECHILD) || pid == 0 {
				break
			}
			if err != nil {
				return err
			}
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		pending := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err == nil && pid > 1 {
				pending = true
				break
			}
		}
		if !pending {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
