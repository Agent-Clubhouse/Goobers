package temporaltest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStartDevServerRejectsNonExecutableExistingPathWithoutNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temporal")
	if err := os.WriteFile(path, []byte("not an executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(temporaltest.CLIEnvVar, path)

	_, err := temporaltest.StartDevServer(context.Background(), t, testsuite.DevServerOptions{})
	if err == nil {
		t.Fatal("StartDevServer with a non-executable GOOBERS_TEMPORAL_CLI path succeeded")
	}
	if strings.Contains(err.Error(), "temporal.download") {
		t.Fatalf("StartDevServer error mentions a network download despite %s being set: %v", temporaltest.CLIEnvVar, err)
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("StartDevServer error = %v, want clear non-executable path error", err)
	}
}
