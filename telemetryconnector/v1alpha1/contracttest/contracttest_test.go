package contracttest_test

import (
	"context"
	"testing"

	connector "github.com/goobers/goobers/telemetryconnector/v1alpha1"
	"github.com/goobers/goobers/telemetryconnector/v1alpha1/contracttest"
)

func TestRunAcceptsConformingConnector(t *testing.T) {
	contracttest.Run(t, conformingConnector{}, connector.QueryRequest{
		Query: "fixture",
		Shape: connector.ShapePoint,
	})
}

type conformingConnector struct{}

func (conformingConnector) Descriptor() connector.Descriptor {
	return connector.Descriptor{Kind: "fixture", Version: "v1", SourceID: "local"}
}

func (conformingConnector) Query(ctx context.Context, _ connector.QueryRequest) (connector.SourceResult, error) {
	if err := ctx.Err(); err != nil {
		return connector.SourceResult{}, err
	}
	return connector.SourceResult{
		Columns: []connector.Column{{Name: "value", Type: connector.TypeBoolean}},
		Rows:    [][]any{{true}},
	}, nil
}
