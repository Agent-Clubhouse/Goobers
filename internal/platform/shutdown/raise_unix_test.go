//go:build unix

package shutdown

import (
	"syscall"
	"testing"
)

// raiseTerm sends SIGTERM to this process so TestNotify_SignalCancels can
// exercise the real OS-signal path. It reports false (skipping the assertion) if
// the environment refuses to deliver the signal.
func raiseTerm(t *testing.T) bool {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Skipf("cannot raise SIGTERM in this environment: %v", err)
		return false
	}
	return true
}
