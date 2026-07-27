package externaltelemetry_test

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/externaltelemetry/contracttest"
)

func TestFakeConnectorContract(t *testing.T) {
	factory := externaltelemetry.FakeFactory{}
	config := externaltelemetry.ConnectorConfig{
		Name:    "fixture",
		Kind:    externaltelemetry.FakeKind,
		Version: externaltelemetry.FakeVersion,
		Config: json.RawMessage(`{
			"source":"checked-in",
			"responses":{
				"health":{
					"columns":[{"name":"healthy","type":"boolean"}],
					"rows":[[true]]
				}
			}
		}`),
	}
	connector, err := factory.Build(config, externaltelemetry.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contracttest.Run(t, connector, externaltelemetry.QueryRequest{
		Query: "health", Shape: externaltelemetry.ShapePoint,
	})
}
