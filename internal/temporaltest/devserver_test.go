package temporaltest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/temporaltest"
)

// TestStartDevServerHonorsExistingPathWithoutNetwork is the regression test
// for #3393: internal/engine and internal/bootstrap tests that need a real
// Temporal dev server had exactly one acquisition path, CachedDownload{
// "default"}, which unconditionally fetches the temporal CLI from
// temporal.download. In an egress-controlled environment that fetch is
// denied and every one of those suites fails before testing anything.
//
// This asserts the offline path actually gets used end to end: with
// GOOBERS_TEMPORAL_CLI set, StartDevServer must fail on *starting* the named
// (here nonexistent) binary rather than attempting any download, proving the
// dev server acquisition never reaches the network when the env var is set.
// The test needs no network access itself and no real Temporal CLI binary.
func TestStartDevServerHonorsExistingPathWithoutNetwork(t *testing.T) {
	bogusPath := "/nonexistent/path/to/temporal-cli-does-not-exist"
	t.Setenv(temporaltest.CLIEnvVar, bogusPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{})
	if err == nil {
		t.Fatal("StartDevServer with a nonexistent GOOBERS_TEMPORAL_CLI path succeeded, want an error starting that binary")
	}
	if strings.Contains(err.Error(), "temporal.download") {
		t.Fatalf("StartDevServer error mentions a network download despite %s being set: %v", temporaltest.CLIEnvVar, err)
	}
}
