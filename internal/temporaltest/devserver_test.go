package temporaltest_test

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStartDevServerRejectsExistingDirectoryWithoutNetwork(t *testing.T) {
	path := t.TempDir()
	t.Setenv(temporaltest.CLIEnvVar, path)

	_, err := temporaltest.StartDevServer(context.Background(), t, testsuite.DevServerOptions{})
	if err == nil {
		t.Fatal("StartDevServer with a directory GOOBERS_TEMPORAL_CLI path succeeded")
	}
	if strings.Contains(err.Error(), "temporal.download") {
		t.Fatalf("StartDevServer error mentions a network download despite %s being set: %v", temporaltest.CLIEnvVar, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("StartDevServer error = %v, want clear regular-file error", err)
	}
}
