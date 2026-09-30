//go:build !windows

package telemetry

import "testing"

func lockReplayFileForTest(t testing.TB, _ string) func() {
	t.Helper()
	return func() {}
}
