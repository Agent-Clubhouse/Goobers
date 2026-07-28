// Package contracttest provides connector checks shared by built-in and
// independently maintained extension adapters.
package contracttest

import (
	"context"
	"testing"

	connector "github.com/goobers/goobers/telemetryconnector/v1alpha1"
)

// Run verifies the common connector identity, result, and cancellation contract.
func Run(t *testing.T, implementation connector.Connector, request connector.QueryRequest) {
	t.Helper()
	descriptor := implementation.Descriptor()
	if descriptor.Kind == "" || descriptor.Version == "" || descriptor.SourceID == "" {
		t.Fatalf("incomplete connector descriptor: %+v", descriptor)
	}
	result, err := implementation.Query(context.Background(), request)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for i, row := range result.Rows {
		if len(row) != len(result.Columns) {
			t.Fatalf("row %d has %d cells for %d columns", i, len(row), len(result.Columns))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := implementation.Query(ctx, request); err == nil {
		t.Fatal("Query with canceled context unexpectedly succeeded")
	}
}
