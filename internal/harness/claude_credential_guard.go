package harness

import (
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// The guards in this file keep `go test` away from the invoking user's real
// Claude Code login (#7447). Run seeds a stored login into each run's private
// CLAUDE_CONFIG_DIR, from <config dir>/.credentials.json or, on macOS, from
// the login Keychain. A unit test that reached either real source would copy
// the developer's own credential into a temp dir, and in a locked or
// sandboxed session would fail on security(1) for reasons unrelated to the
// code under test. Both guards are inert outside a test binary.

// operatorClaudeLoginOptIn lifts both guards for the opt-in live integration
// tests, which drive the real claude CLI and so must seed the operator's real
// stored login exactly as production does. Only optInOperatorClaudeLogin, in
// an integration-tagged test file, sets it.
var operatorClaudeLoginOptIn atomic.Bool

// keychainReader is the reader Run seeds credentials with. A nil
// KeychainReader means the real login Keychain in production, and is refused
// under `go test`. The refusal fires on every platform, not only on darwin
// where the reader is actually consulted, so Linux CI also catches a test
// that forgot to inject one.
func (c *ClaudeAdapter) keychainReader() ClaudeKeychainReader {
	if c.KeychainReader != nil {
		return c.KeychainReader
	}
	if testing.Testing() && !operatorClaudeLoginOptIn.Load() {
		panic("harness: claude-code: ClaudeAdapter.KeychainReader is nil under go test; " +
			"inject a fake reader instead of reading the developer's real macOS Keychain (#7447)")
	}
	return readOperatorClaudeKeychain
}

// refuseOperatorClaudeKeychainUnderTest is the backstop inside the real
// reader itself: it panics before security(1) runs, so a test that names
// readOperatorClaudeKeychain directly (as a KeychainReader, a seeding
// argument, or a plain call) fails as loudly as one that left the field nil.
func refuseOperatorClaudeKeychainUnderTest() {
	if testing.Testing() && !operatorClaudeLoginOptIn.Load() {
		panic("harness: claude-code: the real macOS Keychain reader was called under go test; " +
			"inject a fake ClaudeKeychainReader instead (#7447)")
	}
}

// hostClaudeConfigDirs are the invoking user's real Claude Code config
// directories, captured when a test binary initializes (before any test's
// t.Setenv can move HOME) and empty in every other binary.
var hostClaudeConfigDirs = captureHostClaudeConfigDirs()

func captureHostClaudeConfigDirs() []string {
	if !testing.Testing() {
		return nil
	}
	var dirs []string
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		dirs = append(dirs, filepath.Clean(dir))
	}
	for _, name := range []string{"HOME", "USERPROFILE"} {
		if home := os.Getenv(name); home != "" {
			dirs = append(dirs, filepath.Join(home, ".claude"))
		}
	}
	return dirs
}

// isHostClaudeConfigDir reports whether dir is one of hostClaudeConfigDirs.
func isHostClaudeConfigDir(dir string) bool {
	return slices.Contains(hostClaudeConfigDirs, filepath.Clean(dir))
}

// refuseHostClaudeConfigDirUnderTest is the file-side twin of the
// KeychainReader guard: it panics before a test binary reads the invoking
// user's real .credentials.json, which is where Claude Code keeps its login
// on Linux and Windows. A test that needs a stored login plants one under a
// temp HOME instead (stubClaudeCredentialsHome).
func refuseHostClaudeConfigDirUnderTest(dir string) {
	if !operatorClaudeLoginOptIn.Load() && isHostClaudeConfigDir(dir) {
		panic("harness: claude-code: test would seed credentials from the host's real Claude Code config dir " +
			dir + "; point HOME (or CLAUDE_CONFIG_DIR) at a temp dir (#7447)")
	}
}
