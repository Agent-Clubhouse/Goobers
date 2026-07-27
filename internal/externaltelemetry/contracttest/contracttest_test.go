package contracttest

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/externaltelemetry"
)

func TestRunAcceptsConformingConnector(t *testing.T) {
	Run(t, conformingConnector{}, externaltelemetry.QueryRequest{
		Query: "fixture",
		Shape: externaltelemetry.ShapePoint,
	})
}

type conformingConnector struct{}

func (conformingConnector) Descriptor() externaltelemetry.Descriptor {
	return externaltelemetry.Descriptor{Kind: "fixture", Version: "v1", SourceID: "local"}
}

func (conformingConnector) Query(ctx context.Context, _ externaltelemetry.QueryRequest) (externaltelemetry.SourceResult, error) {
	if err := ctx.Err(); err != nil {
		return externaltelemetry.SourceResult{}, err
	}
	return externaltelemetry.SourceResult{
		Columns: []externaltelemetry.Column{{Name: "value", Type: externaltelemetry.TypeBoolean}},
		Rows:    [][]any{{true}},
	}, nil
}
