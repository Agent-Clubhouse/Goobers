package externaltelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHostNormalizesFreshnessSchemaAndLimits(t *testing.T) {
	dataAsOf := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	registry := configuredFakeRegistry(t, ConnectorConfig{
		Name:    "metrics",
		Kind:    FakeKind,
		Version: FakeVersion,
		Policy: PolicyConfig{
			MaxRows:  2,
			MaxBytes: 4096,
		},
		Config: mustJSON(t, fakeConfig{
			Source: "fixture/service",
			Responses: map[string]SourceResult{
				"latency": {
					Columns:  []Column{{Name: "at", Type: TypeDateTime}, {Name: "latency", Type: TypeNumber, Unit: "ms"}},
					Rows:     [][]any{{"2026-07-24T10:00:00Z", 12.5}, {"2026-07-24T10:01:00Z", 14.0}, {"2026-07-24T10:02:00Z", 13.0}},
					DataAsOf: &dataAsOf,
					Labels:   map[string]string{"region": "west"},
				},
			},
		}),
	})
	now := time.Date(2026, 7, 24, 10, 10, 0, 0, time.UTC)
	host := Host{Registry: registry, Now: func() time.Time { return now }}
	expected := []Column{{Name: "at", Type: TypeDateTime}, {Name: "latency", Type: TypeNumber, Unit: "ms"}}

	artifact, err := host.Query(context.Background(), "metrics", QueryRequest{
		Query:           "latency",
		QueryRef:        "queries/latency.kql",
		Parameters:      map[string]any{"region": "west"},
		ExpectedColumns: expected,
		Shape:           ShapeTimeSeries,
		Freshness:       5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if artifact.SchemaVersion != ResultSchemaVersion || artifact.State != DataStale {
		t.Fatalf("artifact identity/state = %q/%q", artifact.SchemaVersion, artifact.State)
	}
	if !artifact.Metadata.Truncated || artifact.Metadata.RowsDropped != 1 || len(artifact.Rows) != 2 {
		t.Fatalf("limits metadata/rows = %+v / %d", artifact.Metadata, len(artifact.Rows))
	}
	if artifact.Query.Reference != "queries/latency.kql" || artifact.Query.ParameterDigest == "" ||
		len(artifact.Query.ParameterNames) != 1 || artifact.Query.ParameterNames[0] != "region" {
		t.Fatalf("query provenance = %+v", artifact.Query)
	}
	if artifact.Labels["region"] != "west" || artifact.DataAsOf == nil {
		t.Fatalf("source metadata = labels=%v dataAsOf=%v", artifact.Labels, artifact.DataAsOf)
	}
}

func TestHostFailsWhenPointValueExceedsByteLimit(t *testing.T) {
	connector := &sequenceConnector{result: SourceResult{
		Columns: []Column{{Name: "value", Type: TypeString}},
		Rows:    [][]any{{strings.Repeat("large-point-value", MinimumMaxBytes)}},
	}}
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 1, MaxBytes: MinimumMaxBytes,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query: "q", Shape: ShapePoint,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "result_too_large" {
		t.Fatalf("oversized point = state %q failure %+v err %v", artifact.State, artifact.Failure, err)
	}
	if len(artifact.Rows) != 0 {
		t.Fatalf("failed point retained rows: %+v", artifact.Rows)
	}
}

func TestHostDistinguishesEmptyAndFailed(t *testing.T) {
	registry := configuredFakeRegistry(t, ConnectorConfig{
		Name:    "metrics",
		Kind:    FakeKind,
		Version: FakeVersion,
		Config: json.RawMessage(`{
			"source":"fixture",
			"responses":{
				"empty":{"columns":[{"name":"value","type":"integer"}],"rows":[]}
			}
		}`),
	})
	host := Host{Registry: registry}

	empty, err := host.Query(context.Background(), "metrics", QueryRequest{
		Query: "empty", Shape: ShapeTable,
		ExpectedColumns: []Column{{Name: "value", Type: TypeInteger}},
	})
	if err != nil || empty.State != DataEmpty || empty.Failure != nil {
		t.Fatalf("empty query = state %q failure %+v err %v", empty.State, empty.Failure, err)
	}

	failed, err := host.Query(context.Background(), "metrics", QueryRequest{
		Query: "empty", Shape: ShapeTable,
		ExpectedColumns: []Column{{Name: "value", Type: TypeString}},
	})
	if err == nil || failed.State != DataFailed || failed.Failure == nil || failed.Failure.Code != "schema_mismatch" {
		t.Fatalf("failed query = state %q failure %+v err %v", failed.State, failed.Failure, err)
	}
	encoded, marshalErr := json.Marshal(failed)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "actual columns") {
		t.Fatalf("failed artifact persisted error details: %s", encoded)
	}
}

func TestHostRejectsMalformedDatetimeValues(t *testing.T) {
	registry := NewRegistry()
	registry.connectors["metrics"] = configuredConnector{
		config: ConnectorConfig{Name: "metrics", Kind: "sequence", Version: "v1"},
		connector: &sequenceConnector{result: SourceResult{
			Columns: []Column{{Name: "at", Type: TypeDateTime}, {Name: "latency", Type: TypeNumber}},
			Rows:    [][]any{{"not-a-timestamp", 12.5}},
		}},
		shapes: []ResultShape{ShapeTimeSeries},
		limits: QueryLimits{
			Timeout: 5 * time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond, MaxRows: 10, MaxBytes: 4096,
		},
	}

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "metrics", QueryRequest{
		Query: "latency",
		Shape: ShapeTimeSeries,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "invalid_source_value" || artifact.Failure.Kind != "normalization" {
		t.Fatalf("malformed datetime = state %q failure %+v err %v", artifact.State, artifact.Failure, err)
	}
}

func TestHostRejectsUnsignedIntegerOverflow(t *testing.T) {
	connector := &sequenceConnector{result: SourceResult{
		Columns: []Column{{Name: "value", Type: TypeInteger}},
		Rows:    [][]any{{uint64(math.MaxUint64)}},
	}}
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: 5 * time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: 4096,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query: "value",
		Shape: ShapePoint,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "invalid_source_value" || artifact.Failure.Kind != "normalization" {
		t.Fatalf("unsigned overflow = state %q failure %+v err %v", artifact.State, artifact.Failure, err)
	}
}

func TestHostRejectsPointWithoutColumn(t *testing.T) {
	connector := &sequenceConnector{result: SourceResult{
		Rows: [][]any{{}},
	}}
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: 5 * time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: 4096,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query: "value",
		Shape: ShapePoint,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "point_cardinality" || artifact.Failure.Kind != "schema" {
		t.Fatalf("point without column = state %q failure %+v err %v", artifact.State, artifact.Failure, err)
	}
}

func TestHostRejectsEmptyTimeSeriesWithoutTimestampColumn(t *testing.T) {
	registry := NewRegistry()
	registry.connectors["metrics"] = configuredConnector{
		config: ConnectorConfig{Name: "metrics", Kind: "sequence", Version: "v1"},
		connector: &sequenceConnector{result: SourceResult{
			Columns: []Column{{Name: "latency", Type: TypeNumber}},
			Rows:    [][]any{},
		}},
		shapes: []ResultShape{ShapeTimeSeries},
		limits: QueryLimits{
			Timeout: 5 * time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond, MaxRows: 10, MaxBytes: 4096,
		},
	}

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "metrics", QueryRequest{
		Query: "latency",
		Shape: ShapeTimeSeries,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "time_series_timestamp_missing" || artifact.Failure.Kind != "schema" {
		t.Fatalf("empty time series = state %q failure %+v err %v", artifact.State, artifact.Failure, err)
	}
}

func TestHostRetriesOnlyRetryableFailures(t *testing.T) {
	connector := &sequenceConnector{
		errors: []error{
			NewQueryError("throttled", "transport", true, errors.New("429")),
			NewQueryError("throttled", "transport", true, errors.New("429")),
		},
		result: SourceResult{
			Columns: []Column{{Name: "value", Type: TypeInteger}},
			Rows:    [][]any{{1}},
		},
	}
	registry := NewRegistry()
	registry.connectors["sequence"] = configuredConnector{
		config:    ConnectorConfig{Name: "sequence", Kind: "sequence", Version: "v1"},
		connector: connector,
		shapes:    []ResultShape{ShapePoint},
		limits: QueryLimits{
			Timeout: 5 * time.Second, MaxAttempts: 3, RetryBackoff: time.Millisecond, MaxRows: 10, MaxBytes: 4096,
		},
	}
	var sleeps int
	host := Host{
		Registry: registry,
		Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		},
	}
	artifact, err := host.Query(context.Background(), "sequence", QueryRequest{Query: "q", Shape: ShapePoint})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Metadata.Attempts != 3 || connector.calls != 3 || sleeps != 2 {
		t.Fatalf("attempts/calls/sleeps = %d/%d/%d", artifact.Metadata.Attempts, connector.calls, sleeps)
	}

	connector.calls = 0
	connector.errors = []error{NewQueryError("bad_query", "query", false, errors.New("bad query"))}
	failed, err := host.Query(context.Background(), "sequence", QueryRequest{Query: "q", Shape: ShapePoint})
	if err == nil || connector.calls != 1 || failed.Failure == nil || failed.Failure.Retryable {
		t.Fatalf("non-retryable = calls %d failure %+v err %v", connector.calls, failed.Failure, err)
	}
}

func TestHostRejectsConnectorSuccessAfterTimeout(t *testing.T) {
	finished := make(chan struct{})
	connector := connectorFunc(func(context.Context, QueryRequest) (SourceResult, error) {
		defer close(finished)
		time.Sleep(100 * time.Millisecond) // Intentional slow connector verifies in-flight concurrency accounting.
		return SourceResult{
			Columns: []Column{{Name: "value", Type: TypeInteger}},
			Rows:    [][]any{{1}},
		}, nil
	})
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: 10 * time.Millisecond, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: 4096,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query: "q", Shape: ShapePoint,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "timeout" || artifact.Metadata.Attempts != 1 {
		t.Fatalf("late success = state %q failure %+v attempts %d err %v", artifact.State, artifact.Failure, artifact.Metadata.Attempts, err)
	}
	<-finished
}

func TestHostCancellationDoesNotWaitForIgnoringConnector(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	connector := connectorFunc(func(context.Context, QueryRequest) (SourceResult, error) {
		close(started)
		<-release
		return SourceResult{}, nil
	})
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: 4096,
	})
	ctx, cancel := context.WithCancel(context.Background())
	type queryResult struct {
		artifact ResultArtifact
		err      error
	}
	completed := make(chan queryResult, 1)
	go func() {
		artifact, err := (&Host{Registry: registry}).Query(ctx, "sequence", QueryRequest{Query: "q", Shape: ShapeTable})
		completed <- queryResult{artifact: artifact, err: err}
	}()
	<-started
	cancel()

	select {
	case result := <-completed:
		if result.err == nil || result.artifact.Failure == nil || result.artifact.Failure.Code != "canceled" {
			t.Fatalf("canceled query = failure %+v err %v", result.artifact.Failure, result.err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("host waited for connector that ignored cancellation")
	}
	close(release)
}

func TestHostBoundsFailureArtifacts(t *testing.T) {
	connector := &sequenceConnector{
		errors: []error{NewQueryError("bad_query", "query", false, errors.New("bad query"))},
	}
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: MinimumMaxBytes,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query:    "q",
		QueryRef: strings.Repeat("queries/failure.kql", 100),
		Shape:    ShapeTable,
	})
	if err == nil || artifact.Failure == nil || artifact.Failure.Code != "bad_query" {
		t.Fatalf("failed query = failure %+v err %v", artifact.Failure, err)
	}
	data, marshalErr := json.Marshal(artifact)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if len(data) > MinimumMaxBytes {
		t.Fatalf("failed artifact size = %d, limit %d", len(data), MinimumMaxBytes)
	}
	if !artifact.Metadata.Truncated || artifact.Query.Digest == "" {
		t.Fatalf("bounded failure metadata/provenance = %+v / %+v", artifact.Metadata, artifact.Query)
	}
}

func TestHostBoundsEveryEarlyFailurePath(t *testing.T) {
	largeValue := strings.Repeat("request-controlled-value", 1000)
	connector := &sequenceConnector{}
	configuredRegistry := registryForHostTest(connector, QueryLimits{
		Timeout: time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: MinimumMaxBytes,
	})
	tests := []struct {
		name          string
		host          Host
		connectorName string
		wantCode      string
	}{
		{
			name:          "registry not configured",
			host:          Host{},
			connectorName: largeValue,
			wantCode:      "host_not_configured",
		},
		{
			name:          "connector not found",
			host:          Host{Registry: NewRegistry()},
			connectorName: largeValue,
			wantCode:      "connector_not_found",
		},
		{
			name:          "invalid shape",
			host:          Host{Registry: configuredRegistry},
			connectorName: "sequence",
			wantCode:      "invalid_shape",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact, err := test.host.Query(context.Background(), test.connectorName, QueryRequest{
				Query:    "q",
				QueryRef: largeValue,
				Shape:    ResultShape(largeValue),
				Limits:   QueryLimits{MaxBytes: MinimumMaxBytes},
			})
			if err == nil || artifact.Failure == nil || artifact.Failure.Code != test.wantCode {
				t.Fatalf("failure = %+v err %v", artifact.Failure, err)
			}
			data, marshalErr := json.Marshal(artifact)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if len(data) > MinimumMaxBytes {
				t.Fatalf("failed artifact size = %d, limit %d", len(data), MinimumMaxBytes)
			}
			if !artifact.Metadata.Truncated || strings.Contains(string(data), largeValue) {
				t.Fatalf("unbounded failed artifact: %s", data)
			}
		})
	}
	if connector.calls != 0 {
		t.Fatalf("invalid request reached connector %d time(s)", connector.calls)
	}
}

func TestHostConvertsConnectorPanicToBoundedFailure(t *testing.T) {
	panicValue := strings.Repeat("sensitive panic payload", 1000)
	connector := connectorFunc(func(context.Context, QueryRequest) (SourceResult, error) {
		panic(panicValue)
	})
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: MinimumMaxBytes,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query:    "q",
		QueryRef: strings.Repeat("queries/panic.kql", 1000),
		Shape:    ShapeTable,
	})
	if err == nil || artifact.Failure == nil || artifact.Failure.Code != "plugin_panic" ||
		artifact.Failure.Kind != "plugin" {
		t.Fatalf("panic failure = %+v err %v", artifact.Failure, err)
	}
	data, marshalErr := json.Marshal(artifact)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if len(data) > MinimumMaxBytes || strings.Contains(string(data), panicValue) ||
		strings.Contains(err.Error(), panicValue) {
		t.Fatalf("unbounded or unsanitized panic failure: artifact=%s err=%v", data, err)
	}
}

func TestHostRecoversConnectorPanicAfterTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	panicking := make(chan struct{})
	connector := connectorFunc(func(context.Context, QueryRequest) (SourceResult, error) {
		close(started)
		<-release
		defer close(panicking)
		panic("late plugin panic")
	})
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: 10 * time.Millisecond, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: MinimumMaxBytes,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query: "q", Shape: ShapeTable,
	})
	if err == nil || artifact.Failure == nil || artifact.Failure.Code != "timeout" {
		t.Fatalf("timeout failure = %+v err %v", artifact.Failure, err)
	}
	<-started
	close(release)
	select {
	case <-panicking:
	case <-time.After(time.Second):
		t.Fatal("connector did not reach late panic")
	}
}

func TestHostRejectsUndersizedRequestByteLimit(t *testing.T) {
	connector := &sequenceConnector{}
	registry := registryForHostTest(connector, QueryLimits{
		Timeout: time.Second, MaxAttempts: 1, RetryBackoff: time.Millisecond,
		MaxRows: 10, MaxBytes: 4096,
	})

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query:      "q",
		QueryRef:   "queries/q.kql",
		Parameters: map[string]any{"region": "west"},
		Shape:      ShapeTable,
		Limits: QueryLimits{
			MaxBytes: MinimumMaxBytes - 1,
		},
	})
	if err == nil || artifact.Failure == nil || artifact.Failure.Code != "invalid_limits" {
		t.Fatalf("undersized request limit = failure %+v err %v", artifact.Failure, err)
	}
	if connector.calls != 0 {
		t.Fatalf("undersized request reached connector %d time(s)", connector.calls)
	}
	if artifact.Query.Digest == "" || artifact.Query.Reference != "queries/q.kql" ||
		artifact.Query.ParameterDigest == "" || len(artifact.Query.ParameterNames) != 1 ||
		artifact.Query.ParameterNames[0] != "region" {
		t.Fatalf("undersized request provenance = %+v", artifact.Query)
	}
}

func TestConfigurationRejectsInlineOrMalformedCredentialReferences(t *testing.T) {
	tests := []struct {
		name string
		auth AuthConfig
	}{
		{"missing reference", AuthConfig{Mode: AuthBearerToken}},
		{"both references", AuthConfig{Mode: AuthBearerToken, Token: &CredentialRef{Env: "TOKEN", File: "/secret"}}},
		{"token on identity", AuthConfig{Mode: AuthWorkloadIdentity, Token: &CredentialRef{Env: "TOKEN"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Configuration{Connectors: []ConnectorConfig{{
				Name: "metrics", Kind: FakeKind, Version: FakeVersion, Auth: test.auth, Config: json.RawMessage(`{}`),
			}}}
			if err := config.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}

func TestRegistryRejectsUnknownPluginsAndInvalidAdapterConfig(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(FakeFactory{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(FakeFactory{}); err == nil {
		t.Fatal("duplicate registration unexpectedly succeeded")
	}
	if err := registry.Configure(ConnectorConfig{
		Name: "unknown", Kind: "prometheus", Version: "v1", Config: json.RawMessage(`{}`),
	}, nil, nil); err == nil || !strings.Contains(err.Error(), "no plugin registered") {
		t.Fatalf("unknown plugin error = %v", err)
	}
	if err := registry.Configure(ConnectorConfig{
		Name: "bad", Kind: FakeKind, Version: FakeVersion, Config: json.RawMessage(`{"source":"x","responses":{},"secret":"inline"}`),
	}, nil, nil); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("invalid plugin config error = %v", err)
	}
}

func TestRegistryRejectsInvalidShapeDeclarations(t *testing.T) {
	tests := []struct {
		name   string
		shapes []ResultShape
		want   string
	}{
		{name: "invalid", shapes: []ResultShape{"histogram"}, want: "invalid result shape"},
		{name: "duplicate", shapes: []ResultShape{ShapeTable, ShapeTable}, want: "duplicate result shape"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := NewRegistry().Register(sequenceFactory{shapes: test.shapes})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Register error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHostRejectsShapeNotDeclaredByConnector(t *testing.T) {
	connector := &sequenceConnector{result: SourceResult{
		Columns: []Column{{Name: "value", Type: TypeInteger}},
		Rows:    [][]any{{1}},
	}}
	registry := NewRegistry()
	factory := &sequenceFactory{
		shapes:    []ResultShape{ShapeTable},
		connector: connector,
	}
	if err := registry.Register(factory); err != nil {
		t.Fatal(err)
	}
	factory.shapes[0] = ShapePoint
	if err := registry.Configure(ConnectorConfig{
		Name: "sequence", Kind: "sequence", Version: "v1", Config: json.RawMessage(`{}`),
	}, nil, nil); err != nil {
		t.Fatal(err)
	}

	artifact, err := (&Host{Registry: registry}).Query(context.Background(), "sequence", QueryRequest{
		Query: "q",
		Shape: ShapePoint,
	})
	if err == nil || artifact.State != DataFailed || artifact.Failure == nil ||
		artifact.Failure.Code != "unsupported_shape" || artifact.Failure.Kind != "configuration" {
		t.Fatalf("unsupported shape = state %q failure %+v err %v", artifact.State, artifact.Failure, err)
	}
	if artifact.Metadata.Attempts != 0 || connector.calls != 0 {
		t.Fatalf("unsupported shape reached connector: attempts=%d calls=%d", artifact.Metadata.Attempts, connector.calls)
	}
}

type sequenceFactory struct {
	shapes    []ResultShape
	connector Connector
}

func (f sequenceFactory) Definition() Definition {
	return Definition{
		Kind:                "sequence",
		Version:             "v1",
		ConfigurationSchema: json.RawMessage(`{"type":"object"}`),
		AuthenticationModes: []string{AuthNone},
		QueryLanguage:       "test",
		Shapes:              f.shapes,
	}
}

func (sequenceFactory) ValidateConfig(json.RawMessage) error { return nil }

func (f sequenceFactory) Build(ConnectorConfig, BuildOptions) (Connector, error) {
	if f.connector == nil {
		return &sequenceConnector{}, nil
	}
	return f.connector, nil
}

type sequenceConnector struct {
	calls  int
	errors []error
	result SourceResult
}

func (c *sequenceConnector) Descriptor() Descriptor {
	return Descriptor{Kind: "sequence", Version: "v1", SourceID: "fixture"}
}

func (c *sequenceConnector) Query(context.Context, QueryRequest) (SourceResult, error) {
	c.calls++
	if c.calls <= len(c.errors) {
		return SourceResult{}, c.errors[c.calls-1]
	}
	return c.result, nil
}

func configuredFakeRegistry(t *testing.T, config ConnectorConfig) *Registry {
	t.Helper()
	registry := NewRegistry()
	if err := registry.Register(FakeFactory{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Configure(config, nil, nil); err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHostRequiresRegistry(t *testing.T) {
	host := Host{}
	artifact, err := host.Query(context.Background(), "missing", QueryRequest{Query: "q", Shape: ShapeTable})
	if err == nil || artifact.Failure == nil || artifact.Failure.Code != "host_not_configured" {
		t.Fatalf("nil registry result = failure %+v err %v", artifact.Failure, err)
	}
}

func TestHostPolicyAndNetworkBoundary(t *testing.T) {
	limits, err := (PolicyConfig{
		Timeout: "2s", MaxAttempts: 2, RetryBackoff: "3s", MaxRows: 4, MaxBytes: MinimumMaxBytes,
	}).Limits()
	if err != nil {
		t.Fatal(err)
	}
	if limits.Timeout != 2*time.Second || limits.MaxAttempts != 2 ||
		limits.RetryBackoff != 3*time.Second || limits.MaxRows != 4 || limits.MaxBytes != MinimumMaxBytes {
		t.Fatalf("limits = %+v", limits)
	}
	if _, err := (PolicyConfig{MaxBytes: MinimumMaxBytes - 1}).Limits(); err == nil {
		t.Fatal("undersized failure artifact limit unexpectedly accepted")
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	policy := NetworkPolicy{AllowedHosts: []string{serverURL.Hostname()}, AllowHTTP: true}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	client := policyClient(server.Client(), policy)
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	denied, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(denied); err == nil || !strings.Contains(err.Error(), "network policy denies") {
		t.Fatalf("denied request error = %v", err)
	}
}

func TestRegistryNamesAreSorted(t *testing.T) {
	registry := configuredFakeRegistry(t, ConnectorConfig{
		Name: "zeta", Kind: FakeKind, Version: FakeVersion,
		Config: json.RawMessage(`{"source":"zeta","responses":{"q":{"columns":[],"rows":[]}}}`),
	})
	if err := registry.Configure(ConnectorConfig{
		Name: "alpha", Kind: FakeKind, Version: FakeVersion,
		Config: json.RawMessage(`{"source":"alpha","responses":{"q":{"columns":[],"rows":[]}}}`),
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	names := registry.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Fatalf("Names = %v", names)
	}
}

type connectorFunc func(context.Context, QueryRequest) (SourceResult, error)

func (connectorFunc) Descriptor() Descriptor {
	return Descriptor{Kind: "sequence", Version: "v1", SourceID: "fixture"}
}

func (f connectorFunc) Query(ctx context.Context, request QueryRequest) (SourceResult, error) {
	return f(ctx, request)
}

func registryForHostTest(connector Connector, limits QueryLimits) *Registry {
	registry := NewRegistry()
	registry.connectors["sequence"] = configuredConnector{
		config:    ConnectorConfig{Name: "sequence", Kind: "sequence", Version: "v1"},
		connector: connector,
		shapes:    []ResultShape{ShapePoint, ShapeTable, ShapeTimeSeries},
		limits:    limits,
	}
	return registry
}

func ExampleResultArtifact() {
	fmt.Println(ResultSchemaVersion)
	// Output: goobers.dev/external-telemetry-query-result/v1alpha1
}
