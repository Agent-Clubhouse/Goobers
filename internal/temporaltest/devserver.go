package temporaltest

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// CLIEnvVar is the environment variable StartDevServer reads to decide how to
// acquire the Temporal CLI binary. See StartDevServer for the two modes.
const CLIEnvVar = "GOOBERS_TEMPORAL_CLI"

// StartDevServer starts a Temporal CLI dev server, centralizing how the
// engine and bootstrap suites that need a real dev server (Schedules and
// worker/bootstrap paths cannot run in the SDK's in-process test environment)
// acquire the temporal CLI binary (#3393).
//
// If CLIEnvVar is set, the named path is used directly via
// DevServerOptions.ExistingPath — an offline path that works in an
// egress-controlled environment and, as a side effect, pins the dev-server
// version instead of floating on whatever "default" currently resolves to on
// temporal.download. If CLIEnvVar is unset, the CLI is fetched and cached via
// DevServerOptions.CachedDownload{Version: "default"} — exactly the behavior
// every call site had before this helper existed, so ordinary CI (which does
// not set CLIEnvVar) is unaffected. Either way, StartDevServer logs which
// mode it used through t, so a run's test output shows whether it acquired
// the CLI online or offline.
//
// opts is passed through to the SDK as-is except for the two acquisition
// fields (ExistingPath and CachedDownload), which StartDevServer overwrites
// according to CLIEnvVar; callers should leave both zero.
func StartDevServer(ctx context.Context, t testing.TB, opts testsuite.DevServerOptions) (*testsuite.DevServer, error) {
	t.Helper()

	existingPath, cached, mode := resolveDevServerAcquisition(os.Getenv(CLIEnvVar))
	opts.ExistingPath = existingPath
	opts.CachedDownload = cached
	t.Logf("temporaltest: %s", mode)

	return testsuite.StartDevServer(ctx, opts)
}

// resolveDevServerAcquisition decides how to acquire the Temporal CLI binary
// given the raw value of CLIEnvVar, returning the ExistingPath/CachedDownload
// pair StartDevServer should set on DevServerOptions plus a human-readable
// description of the mode for logging.
//
// Split out from StartDevServer so the decision — the part #3393 is actually
// about — is testable without spawning a real dev server process or touching
// the network.
func resolveDevServerAcquisition(rawEnv string) (existingPath string, cached testsuite.CachedDownload, mode string) {
	if path := strings.TrimSpace(rawEnv); path != "" {
		return path, testsuite.CachedDownload{}, "starting Temporal dev server from existing CLI (" + CLIEnvVar + "=" + path + ")"
	}
	return "", testsuite.CachedDownload{Version: "default"},
		"starting Temporal dev server via cached download (version=default); set " + CLIEnvVar +
			" to a pre-existing Temporal CLI binary to run offline"
}
