package externaltelemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// FakeKind is the hermetic local connector kind.
	FakeKind = "fake"
	// FakeVersion is the built-in fake connector version.
	FakeVersion = "v1"
)

// FakeFactory constructs checked-in, hermetic query fixtures.
type FakeFactory struct{}

// Definition declares the fake connector contract.
func (FakeFactory) Definition() Definition {
	return Definition{
		Kind:                FakeKind,
		Version:             FakeVersion,
		ConfigurationSchema: json.RawMessage(`{"type":"object","required":["source","responses"],"properties":{"source":{"type":"string"},"responses":{"type":"object"}},"additionalProperties":false}`),
		AuthenticationModes: []string{AuthNone},
		QueryLanguage:       "fixture-key",
		Shapes:              []ResultShape{ShapePoint, ShapeTable, ShapeTimeSeries},
	}
}

type fakeConfig struct {
	Source    string                  `json:"source"`
	Responses map[string]SourceResult `json:"responses"`
}

// ValidateConfig validates fake fixture configuration.
func (FakeFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := decodeFakeConfig(raw)
	return err
}

// Build constructs one fake connector.
func (FakeFactory) Build(config ConnectorConfig, _ BuildOptions) (Connector, error) {
	decoded, err := decodeFakeConfig(config.Config)
	if err != nil {
		return nil, err
	}
	return &fakeConnector{source: decoded.Source, responses: decoded.Responses}, nil
}

func decodeFakeConfig(raw json.RawMessage) (fakeConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var config fakeConfig
	if err := decoder.Decode(&config); err != nil {
		return fakeConfig{}, fmt.Errorf("decode fake config: %w", err)
	}
	if config.Source == "" || len(config.Responses) == 0 {
		return fakeConfig{}, errors.New("source and at least one response are required")
	}
	for query, result := range config.Responses {
		if query == "" {
			return fakeConfig{}, errors.New("response query keys must not be empty")
		}
		if err := validateSourceResult(result, ShapeTable); err != nil {
			return fakeConfig{}, fmt.Errorf("response %q: %w", query, err)
		}
	}
	return config, nil
}

type fakeConnector struct {
	source    string
	responses map[string]SourceResult
}

func (c *fakeConnector) Descriptor() Descriptor {
	return Descriptor{Kind: FakeKind, Version: FakeVersion, SourceID: c.source}
}

func (c *fakeConnector) Query(ctx context.Context, request QueryRequest) (SourceResult, error) {
	if err := ctx.Err(); err != nil {
		return SourceResult{}, err
	}
	result, exists := c.responses[request.Query]
	if !exists {
		return SourceResult{}, NewQueryError(
			"fixture_not_found",
			"query",
			false,
			fmt.Errorf("fake connector has no response for query %q", request.Query),
		)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return SourceResult{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var cloned SourceResult
	if err := decoder.Decode(&cloned); err != nil {
		return SourceResult{}, err
	}
	return cloned, nil
}
