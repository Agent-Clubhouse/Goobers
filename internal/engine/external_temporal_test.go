package engine

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/temporaltest"
)

func startReplayClient(ctx context.Context, t testing.TB, opts testsuite.DevServerOptions) (client.Client, func() error, error) {
	t.Helper()
	if address := strings.TrimSpace(os.Getenv(temporaltest.ServerAddressEnvVar)); address != "" {
		t.Logf("connecting replay test to external Temporal server (%s=%s)", temporaltest.ServerAddressEnvVar, address)
		c, err := client.Dial(client.Options{HostPort: address, Namespace: "default"})
		if err != nil {
			return nil, nil, err
		}
		return c, func() error {
			c.Close()
			return nil
		}, nil
	}
	server, err := temporaltest.StartDevServer(ctx, t, opts)
	if err != nil {
		return nil, nil, err
	}
	return server.Client(), server.Stop, nil
}
