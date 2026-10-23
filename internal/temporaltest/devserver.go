package temporaltest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
// code change. v1.9.0 carries Server 1.31 for the Worker Deployment GA ladder.
// Bump deliberately, alongside the hosted CI pin.
const CLIVersion = temporalcli.Version

// StartDevServer starts a Temporal CLI dev server, centralizing how the
// engine and bootstrap suites that need a real dev server (Schedules and
// worker/bootstrap paths cannot run in the SDK's in-process test environment)
// acquire the temporal CLI binary (#3393).
//
// If CLIEnvVar names an existing executable, it is used directly via
// DevServerOptions.ExistingPath. If that path is missing, the pinned CLI is
// fetched there with bounded retry; an existing non-executable path fails
// clearly instead of reaching process startup. If CLIEnvVar is unset, the same
// downloader uses the system temporary directory. StartDevServer logs which
// mode it used through t.
//
// opts is passed through to the SDK as-is except for the two acquisition
// fields (ExistingPath and CachedDownload), which StartDevServer overwrites
// according to CLIEnvVar; callers should leave both zero.
func StartDevServer(ctx context.Context, t testing.TB, opts testsuite.DevServerOptions) (*testsuite.DevServer, error) {
	t.Helper()

	acquisition, err := resolveDevServerAcquisition(os.Getenv(CLIEnvVar))
	if err != nil {
		return nil, err
	}
	t.Logf("temporaltest: %s", acquisition.mode)
	if acquisition.download {
		if err := temporalcli.Download(ctx, acquisition.path); err != nil {
			return nil, err
		}
	}
	opts.ExistingPath = acquisition.path
	opts.CachedDownload = testsuite.CachedDownload{}

	return testsuite.StartDevServer(ctx, opts)
}

type devServerAcquisition struct {
	path     string
	mode     string
	download bool
}

// resolveDevServerAcquisition decides how to acquire the Temporal CLI binary
// given the raw value of CLIEnvVar.
//
// Split out from StartDevServer so the decision — the part #3393 is actually
// about — is testable without spawning a real dev server process or touching
// the network.
func resolveDevServerAcquisition(rawEnv string) (devServerAcquisition, error) {
	path := strings.TrimSpace(rawEnv)
	configured := path != ""
	if !configured {
		path = filepath.Join(os.TempDir(), "temporal-cli-"+CLIVersion)
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		mode := "starting Temporal dev server via retrying cached download (version=" + CLIVersion + ")"
		if configured {
			mode = "configured Temporal CLI is missing; downloading pinned CLI to " + CLIEnvVar + "=" + path
		}
		return devServerAcquisition{path: path, mode: mode, download: true}, nil
	}
	if err != nil {
		return devServerAcquisition{}, fmt.Errorf("inspect %s=%q: %w", CLIEnvVar, path, err)
	}
	if !info.Mode().IsRegular() {
		return devServerAcquisition{}, fmt.Errorf("%s=%q is not a regular file", CLIEnvVar, path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return devServerAcquisition{}, fmt.Errorf("%s=%q is not executable", CLIEnvVar, path)
	}
	mode := "starting Temporal dev server from cached CLI (version=" + CLIVersion + ")"
	if configured {
		mode = "starting Temporal dev server from existing CLI (" + CLIEnvVar + "=" + path + ")"
	}
	return devServerAcquisition{path: path, mode: mode}, nil
}
