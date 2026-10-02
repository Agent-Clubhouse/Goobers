package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/sdk/metric"
)

// Reader registration and collection remain SDK-owned; the client invokes
// lifecycle operations concurrently so the SDK's sequential reader loop cannot
// spend every destination's deadline on the first failed endpoint.
type ownedMetricReader struct{ metric.Reader }

func (ownedMetricReader) Shutdown(context.Context) error   { return nil }
func (ownedMetricReader) ForceFlush(context.Context) error { return nil }

func (c *Client) startNamedMetricFlush(ctx context.Context) func() {
	if len(c.namedMetricReaders) == 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = parallelDestinationCalls(len(c.namedMetricReaders), func(i int) error {
			if reader, ok := c.namedMetricReaders[i].(interface{ ForceFlush(context.Context) error }); ok {
				return reader.ForceFlush(ctx)
			}
			return nil
		})
	}()
	return func() { <-done }
}

func (c *Client) shutdownNamedMetrics(ctx context.Context) {
	_ = parallelDestinationCalls(len(c.namedMetricReaders), func(i int) error { return c.namedMetricReaders[i].Shutdown(ctx) })
}
