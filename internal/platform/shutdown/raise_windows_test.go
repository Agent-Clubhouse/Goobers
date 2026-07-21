//go:build windows

package shutdown

import "testing"

// raiseTerm cannot deliver a shutdown signal to the current process on windows —
// os/signal only observes console Ctrl+C, which a test cannot synthesize for
// itself — so it always skips. The service-stop path is covered by the
// RequestStop tests, which run on every platform.
func raiseTerm(t *testing.T) bool {
	t.Helper()
	t.Skip("windows cannot raise an OS shutdown signal to itself; RequestStop covers the trigger")
	return false
}
