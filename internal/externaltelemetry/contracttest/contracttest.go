// Package contracttest provides the connector checks shared by built-in and
// extension adapters.
package contracttest

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/externaltelemetry"
)

// Run verifies the common connector identity, result, and cancellation contract.
func Run(t *testing.T, connector externaltelemetry.Connector, request externaltelemetry.QueryRequest) {
	t.Helper()
	descriptor := connector.Descriptor()
	if descriptor.Kind == "" || descriptor.Version == "" || descriptor.SourceID == "" {
		t.Fatalf("incomplete connector descriptor: %+v", descriptor)
	}
	result, err := connector.Query(context.Background(), request)
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
	if _, err := connector.Query(ctx, request); err == nil {
		t.Fatal("Query with canceled context unexpectedly succeeded")
	}
}
