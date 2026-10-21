package temporaltest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/temporalcli"
)

// CLIEnvVar is the environment variable StartDevServer reads to decide how to
// acquire the Temporal CLI binary. See StartDevServer for the two modes.
const CLIEnvVar = temporalcli.EnvVar

// CLIVersion pins the Temporal CLI the cached download fetches. "default" on
// temporal.download floats to the newest release: v1.9.1 (Server 1.32.0),
// published 2026-09-14, rejects this suite's schedule reconciliation with
// "Not enough time to schedule next run", failing every PR's CI without a
// code change. Bump deliberately, alongside the linux-ci image pin.
const CLIVersion = temporalcli.Version

// StartDevServer starts a Temporal CLI dev server, centralizing how the
// engine and bootstrap suites that need a real dev server (Schedules and
// worker/bootstrap paths cannot run in the SDK's in-process test environment)
// acquire the temporal CLI binary (#3393).
//
// If CLIEnvVar is set, the named path is used directly via
// DevServerOptions.ExistingPath — an offline path that works in an
// egress-controlled environment and, as a side effect, pins the dev-server
// version instead of floating on whatever "default" currently resolves to on
// temporal.download. If CLIEnvVar is unset, the pinned CLI is fetched with
// bounded retry into the system temporary directory. Either way,
// StartDevServer logs which mode it used through t.
//
// opts is passed through to the SDK as-is except for the two acquisition
// fields (ExistingPath and CachedDownload), which StartDevServer overwrites
// according to CLIEnvVar; callers should leave both zero.
func StartDevServer(ctx context.Context, t testing.TB, opts testsuite.DevServerOptions) (*testsuite.DevServer, error) {
	t.Helper()

	existingPath, mode := resolveDevServerAcquisition(os.Getenv(CLIEnvVar))
	if existingPath == "" {
		existingPath = filepath.Join(os.TempDir(), "temporal-cli-"+CLIVersion)
		if err := temporalcli.Download(ctx, existingPath); err != nil {
			return nil, err
		}
		mode = "starting Temporal dev server via retrying cached download (version=" + CLIVersion + ")"
	}
	opts.ExistingPath = existingPath
	opts.CachedDownload = testsuite.CachedDownload{}
	t.Logf("temporaltest: %s", mode)

	return testsuite.StartDevServer(ctx, opts)
}

// resolveDevServerAcquisition decides how to acquire the Temporal CLI binary
// given the raw value of CLIEnvVar, returning the path to use, if any, plus a
// human-readable description of the mode for logging.
//
// Split out from StartDevServer so the decision — the part #3393 is actually
// about — is testable without spawning a real dev server process or touching
// the network.
func resolveDevServerAcquisition(rawEnv string) (existingPath, mode string) {
	if path := strings.TrimSpace(rawEnv); path != "" {
		return path, "starting Temporal dev server from existing CLI (" + CLIEnvVar + "=" + path + ")"
	}
	return "",
		"starting Temporal dev server via cached download (version=" + CLIVersion + "); set " + CLIEnvVar +
			" to a pre-existing Temporal CLI binary to run offline"
}
