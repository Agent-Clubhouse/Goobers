package v1alpha1

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRegisterFactoryReturnsDeterministicSnapshot(t *testing.T) {
	extensionFactories.Lock()
	extensionFactories.byKey = make(map[string]Factory)
	extensionFactories.Unlock()
	t.Cleanup(func() {
		extensionFactories.Lock()
		extensionFactories.byKey = make(map[string]Factory)
		extensionFactories.Unlock()
	})

	if err := RegisterFactory(testFactory{kind: "zeta"}); err != nil {
		t.Fatal(err)
	}

	if err := RegisterFactory(testFactory{kind: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterFactory(testFactory{kind: "alpha"}); err == nil {
		t.Fatal("duplicate factory registration unexpectedly succeeded")
	}
	factories := RegisteredFactories()
	if len(factories) != 2 ||
		factories[0].Definition().Kind != "alpha" ||
		factories[1].Definition().Kind != "zeta" {
		t.Fatalf("factories = %+v", factories)
	}
}

func TestNewQueryErrorExposesRetryClassification(t *testing.T) {
	err := NewQueryError("throttled", "transport", true, errors.New("retry later"))
	var queryErr *QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != "throttled" || !queryErr.Retryable {
		t.Fatalf("query error = %#v", err)
	}
}

type testFactory struct {
	kind string
}

func (f testFactory) Definition() Definition {
	return Definition{
		Kind:                f.kind,
		Version:             "v1",
		ConfigurationSchema: json.RawMessage(`{"type":"object"}`),
		AuthenticationModes: []string{AuthNone},
		QueryLanguage:       "test",
		Shapes:              []ResultShape{ShapeTable},
	}
}

func (testFactory) ValidateConfig(json.RawMessage) error { return nil }

func (f testFactory) Build(ConnectorConfig, BuildOptions) (Connector, error) {
	return testConnector(f), nil
}

type testConnector struct {
	kind string
}

func (c testConnector) Descriptor() Descriptor {
	return Descriptor{Kind: c.kind, Version: "v1", SourceID: "test"}
}

func (testConnector) Query(context.Context, QueryRequest) (SourceResult, error) {
	return SourceResult{}, nil
}
