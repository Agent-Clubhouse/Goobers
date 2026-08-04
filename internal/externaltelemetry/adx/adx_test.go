package adx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/telemetryconnector/v1alpha1/contracttest"
)

const successResponse = `[
	{"FrameType":"DataSetHeader","IsProgressive":false},
	{"FrameType":"DataTable","TableId":0,"TableKind":"PrimaryResult","TableName":"PrimaryResult",
	 "Columns":[{"ColumnName":"at","ColumnType":"datetime"},{"ColumnName":"latency","ColumnType":"real"}],
	 "Rows":[["2026-07-24T10:00:00Z",12.5]]},
	{"FrameType":"DataSetCompletion","HasErrors":false}
]`

func TestADXConnectorContractAndParameterizedRequest(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v2/rest/query" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Error("fixture request unexpectedly carried authorization")
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("x-ms-data-as-of", "2026-07-24T10:01:00Z")
		_, _ = writer.Write([]byte(successResponse))
	}))
	defer server.Close()

	connector := buildTestConnector(t, server, externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone}, nil)
	windowStart := time.Date(2026, 7, 24, 9, 45, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	request := externaltelemetry.QueryRequest{
		Query: "declare query_parameters(region:string, count:long, threshold:real, healthy:bool, labels:dynamic); " +
			"Metrics | where Region == region",
		Parameters: map[string]any{
			"region":    "west",
			"count":     json.Number("10"),
			"threshold": json.Number("12.5"),
			"healthy":   true,
			"labels":    map[string]any{"service": "api"},
		},
		Window: externaltelemetry.Window{
			Start: &windowStart,
			End:   &windowEnd,
		},
		Shape: externaltelemetry.ShapeTimeSeries,
		Limits: externaltelemetry.QueryLimits{
			MaxBytes: 4096,
		},
	}
	contracttest.Run(t, connector, request)
	if requestBody["db"] != "metrics" {
		t.Fatalf("database = %v", requestBody["db"])
	}
	propertiesJSON, ok := requestBody["properties"].(string)
	if !ok {
		t.Fatalf("properties = %#v", requestBody["properties"])
	}
	var properties map[string]any
	if err := json.Unmarshal([]byte(propertiesJSON), &properties); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	wantProperties := map[string]any{
		"Options": map[string]any{
			"request_callout_disabled":             true,
			"request_external_data_disabled":       true,
			"request_external_table_disabled":      true,
			"request_impersonation_disabled":       true,
			"request_readonly_hardline":            true,
			"request_remote_entities_disabled":     true,
			"request_sandboxed_execution_disabled": true,
		},
		"Parameters": map[string]any{
			"region":             "west",
			"count":              float64(10),
			"threshold":          "real(12.5)",
			"healthy":            "bool(true)",
			"labels":             `dynamic({"service":"api"})`,
			WindowStartParameter: "datetime(2026-07-24T09:45:00Z)",
			WindowEndParameter:   "datetime(2026-07-24T10:00:00Z)",
		},
	}
	if !reflect.DeepEqual(properties, wantProperties) {
		t.Fatalf("properties = %#v, want %#v", properties, wantProperties)
	}
}

func TestRequestPayloadIncludesRestrictionsWithoutParameters(t *testing.T) {
	payload, err := requestPayload(context.Background(), "metrics", externaltelemetry.QueryRequest{Query: "Metrics | take 1"})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Properties string `json:"properties"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	var properties map[string]any
	if err := json.Unmarshal([]byte(body.Properties), &properties); err != nil {
		t.Fatal(err)
	}
	if _, exists := properties["Parameters"]; exists {
		t.Fatalf("parameter-free properties unexpectedly contain Parameters: %#v", properties)
	}
	options, ok := properties["Options"].(map[string]any)
	if !ok || options["request_readonly_hardline"] != true ||
		options["request_callout_disabled"] != true ||
		options["request_external_data_disabled"] != true ||
		options["request_remote_entities_disabled"] != true {
		t.Fatalf("request restrictions = %#v", properties["Options"])
	}
	if _, exists := options["servertimeout"]; exists {
		t.Fatalf("request without a deadline contains servertimeout: %#v", options)
	}
}

func TestADXRequestIncludesServerTimeoutFromDeadline(t *testing.T) {
	timeoutValue := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Properties string `json:"properties"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var properties struct {
			Options map[string]any `json:"Options"`
		}
		if err := json.Unmarshal([]byte(body.Properties), &properties); err != nil {
			t.Errorf("decode properties: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		timeout, _ := properties.Options["servertimeout"].(string)
		timeoutValue <- timeout
		_, _ = writer.Write([]byte(successResponse))
	}))
	defer server.Close()

	connector := buildTestConnector(t, server, externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := connector.Query(ctx, externaltelemetry.QueryRequest{
		Query:  "Metrics | take 1",
		Shape:  externaltelemetry.ShapeTimeSeries,
		Limits: externaltelemetry.QueryLimits{MaxBytes: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	timeout := <-timeoutValue
	parsed, err := time.Parse("15:04:05", timeout)
	if err != nil {
		t.Fatalf("servertimeout = %q: %v", timeout, err)
	}
	duration := time.Duration(parsed.Hour())*time.Hour +
		time.Duration(parsed.Minute())*time.Minute +
		time.Duration(parsed.Second())*time.Second +
		time.Duration(parsed.Nanosecond())
	if duration < 45*time.Second || duration > time.Minute {
		t.Fatalf("servertimeout = %q (%s), want remaining one-minute context deadline", timeout, duration)
	}
}

func TestFormatADXTimespan(t *testing.T) {
	tests := map[time.Duration]string{
		0:                     "00:00:00",
		100 * time.Nanosecond: "00:00:00.0000001",
		2*time.Hour + 3*time.Minute + 4*time.Second:       "02:03:04",
		25*time.Hour + 2*time.Minute + 3*time.Second + 40: "1.01:02:03",
	}
	for duration, want := range tests {
		if got := formatADXTimespan(duration); got != want {
			t.Errorf("formatADXTimespan(%s) = %q, want %q", duration, got, want)
		}
	}
}

func TestEncodeParameterScalarTypesAndFailures(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    any
		wantErr bool
	}{
		{name: "nil", value: nil, want: "dynamic(null)"},
		{name: "string", value: "west", want: "west"},
		{name: "integer number", value: json.Number("12"), want: json.Number("12")},
		{name: "real number", value: json.Number("12.5"), want: "real(12.5)"},
		{name: "invalid number", value: json.Number("x"), wantErr: true},
		{name: "boolean", value: true, want: "bool(true)"},
		{name: "float32", value: float32(1.5), want: "real(1.5)"},
		{name: "float64", value: 2.5, want: "real(2.5)"},
		{name: "int", value: int(1), want: int(1)},
		{name: "int8", value: int8(2), want: int8(2)},
		{name: "int16", value: int16(3), want: int16(3)},
		{name: "int32", value: int32(4), want: int32(4)},
		{name: "int64", value: int64(5), want: int64(5)},
		{name: "uint", value: uint(6), want: int64(6)},
		{name: "uint8", value: uint8(7), want: int64(7)},
		{name: "uint16", value: uint16(8), want: int64(8)},
		{name: "uint32", value: uint32(9), want: int64(9)},
		{name: "uint64", value: uint64(10), want: int64(10)},
		{name: "dynamic object", value: map[string]any{"service": "api"}, want: `dynamic({"service":"api"})`},
		{name: "dynamic array", value: []any{"api"}, want: `dynamic(["api"])`},
		{name: "dynamic raw", value: json.RawMessage(`{"service":"api"}`), want: `dynamic({"service":"api"})`},
		{name: "nan", value: math.NaN(), wantErr: true},
		{name: "infinity", value: math.Inf(1), wantErr: true},
		{name: "unsigned overflow", value: uint64(math.MaxUint64), wantErr: true},
		{name: "unsupported", value: struct{}{}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := encodeParameter(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("encodeParameter(%T) unexpectedly succeeded: %v", test.value, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("encodeParameter(%T) = %#v, want %#v", test.value, got, test.want)
			}
		})
	}
}

func TestNormalizeColumnTypeVocabulary(t *testing.T) {
	tests := map[string]externaltelemetry.ColumnType{
		"guid":     externaltelemetry.TypeString,
		"int32":    externaltelemetry.TypeInteger,
		"decimal":  externaltelemetry.TypeNumber,
		"boolean":  externaltelemetry.TypeBoolean,
		"date":     externaltelemetry.TypeDateTime,
		"timespan": externaltelemetry.TypeDuration,
		"dynamic":  externaltelemetry.TypeJSON,
	}
	for source, want := range tests {
		got, err := normalizeColumnType(source)
		if err != nil {
			t.Fatalf("normalizeColumnType(%q): %v", source, err)
		}
		if got != want {
			t.Fatalf("normalizeColumnType(%q) = %q, want %q", source, got, want)
		}
	}
	if _, err := normalizeColumnType("geography"); err == nil {
		t.Fatal("unsupported column type unexpectedly succeeded")
	}
}

func TestADXBearerTokenIsRegisteredButNotReturned(t *testing.T) {
	const token = "fixture-token-value"
	t.Setenv("ADX_TEST_TOKEN", token)
	registrar := &recordingRegistrar{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization header was not populated")
		}
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	connector := buildTestConnector(t, server, externaltelemetry.AuthConfig{
		Mode:  externaltelemetry.AuthBearerToken,
		Token: &externaltelemetry.CredentialRef{Env: "ADX_TEST_TOKEN"},
	}, registrar)
	_, err := connector.Query(context.Background(), externaltelemetry.QueryRequest{
		Query: "Metrics | take 1", Shape: externaltelemetry.ShapeTable,
		Limits: externaltelemetry.QueryLimits{MaxBytes: 4096},
	})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("query error = %v", err)
	}
	if !registrar.contains(token) {
		t.Fatal("resolved token was not registered for redaction")
	}
}

func TestADXHostRetriesThrottleAndRetainsFreshness(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.Header().Set("x-ms-data-as-of", "2026-07-24T10:00:00Z")
		_, _ = writer.Write([]byte(successResponse))
	}))
	defer server.Close()

	registry := configuredRegistry(t, server, externaltelemetry.PolicyConfig{
		Timeout: "2s", MaxAttempts: 2, RetryBackoff: "1ms", MaxRows: 10, MaxBytes: 4096,
	}, externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone}, nil)
	now := time.Date(2026, 7, 24, 10, 10, 0, 0, time.UTC)
	host := externaltelemetry.Host{
		Registry: registry,
		Now:      func() time.Time { return now },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	artifact, err := host.Query(context.Background(), "adx", externaltelemetry.QueryRequest{
		Query: "Metrics | take 1", Shape: externaltelemetry.ShapeTimeSeries, Freshness: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.State != externaltelemetry.DataStale || artifact.Metadata.Attempts != 2 || calls != 2 {
		t.Fatalf("artifact state/attempts/calls = %q/%d/%d", artifact.State, artifact.Metadata.Attempts, calls)
	}
}

func TestADXFailuresAreExplicit(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		policy   externaltelemetry.PolicyConfig
		wantCode string
	}{
		{
			name: "auth",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusForbidden)
			},
			policy:   externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxBytes: 4096},
			wantCode: "authentication_failed",
		},
		{
			name: "oversized",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(writer,
					`{"Tables":[{"TableName":"PrimaryResult","Columns":[{"ColumnName":"value","ColumnType":"string"}],"Rows":[[%q]]}]}`,
					strings.Repeat("x", 70_000),
				)
			},
			policy:   externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxBytes: externaltelemetry.MinimumMaxBytes},
			wantCode: "response_too_large",
		},
		{
			name: "timeout",
			handler: func(_ http.ResponseWriter, _ *http.Request) {
				time.Sleep(100 * time.Millisecond) // Intentional slow response exercises the client timeout.
			},
			policy:   externaltelemetry.PolicyConfig{Timeout: "20ms", MaxAttempts: 1, MaxBytes: 4096},
			wantCode: "timeout",
		},
		{
			name: "completion error after rows",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`[
					{"FrameType":"DataTable","TableKind":"PrimaryResult",
					 "Columns":[{"ColumnName":"value","ColumnType":"long"}],"Rows":[[1]]},
					{"FrameType":"DataSetCompletion","HasErrors":true}
				]`))
			},
			policy:   externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxBytes: 4096},
			wantCode: "invalid_response",
		},
		{
			name: "missing completion after rows",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`[
					{"FrameType":"DataTable","TableKind":"PrimaryResult",
					 "Columns":[{"ColumnName":"value","ColumnType":"long"}],"Rows":[[1]]}
				]`))
			},
			policy:   externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxBytes: 4096},
			wantCode: "invalid_response",
		},
		{
			name: "v1 object response",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`{
					"Tables":[{
						"TableName":"PrimaryResult",
						"Columns":[{"ColumnName":"value","DataType":"long"}],
						"Rows":[[1]]
					}]
				}`))
			},
			policy:   externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxBytes: 4096},
			wantCode: "invalid_response",
		},
		{
			name: "multiple primary results",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`[
					{"FrameType":"DataTable","TableKind":"PrimaryResult",
					 "Columns":[{"ColumnName":"first","ColumnType":"long"}],"Rows":[[1]]},
					{"FrameType":"DataTable","TableKind":"PrimaryResult",
					 "Columns":[{"ColumnName":"second","ColumnType":"long"}],"Rows":[[2]]},
					{"FrameType":"DataSetCompletion","HasErrors":false}
				]`))
			},
			policy:   externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxBytes: 4096},
			wantCode: "invalid_response",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			registry := configuredRegistry(t, server, test.policy, externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone}, nil)
			host := externaltelemetry.Host{Registry: registry}
			artifact, err := host.Query(context.Background(), "adx", externaltelemetry.QueryRequest{
				Query: "Metrics | take 1", Shape: externaltelemetry.ShapeTable,
			})
			if err == nil || artifact.State != externaltelemetry.DataFailed || artifact.Failure == nil || artifact.Failure.Code != test.wantCode {
				t.Fatalf("failure = state %q info %+v err %v", artifact.State, artifact.Failure, err)
			}
		})
	}
}

func TestADXRejectsControlCommandsBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()
	connector := buildTestConnector(t, server, externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone}, nil)
	_, err := connector.Query(context.Background(), externaltelemetry.QueryRequest{
		Query:  ".set-or-append Metrics <| print value=1",
		Shape:  externaltelemetry.ShapeTable,
		Limits: externaltelemetry.QueryLimits{MaxBytes: 4096},
	})
	if err == nil || calls != 0 {
		t.Fatalf("control query error/calls = %v/%d", err, calls)
	}
	var queryErr *externaltelemetry.QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != "read_only_violation" {
		t.Fatalf("error = %#v", err)
	}
}

func TestDecodeResponseRejectsFailedCompletion(t *testing.T) {
	tests := []struct {
		name       string
		completion string
	}{
		{name: "errors", completion: `{"FrameType":"DataSetCompletion","HasErrors":true}`},
		{name: "cancelled", completion: `{"FrameType":"DataSetCompletion","Cancelled":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fmt.Sprintf(`[
				{"FrameType":"DataTable","TableKind":"PrimaryResult","Columns":[{"ColumnName":"value","ColumnType":"long"}],"Rows":[[1]]},
				%s
			]`, test.completion)
			if _, err := decodeResponse([]byte(response), nil); err == nil {
				t.Fatal("failed completion unexpectedly returned data")
			}
		})
	}
}

func TestADXRejectsShortWatermarkRowAsInvalidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`[
			{"FrameType":"DataTable","TableKind":"PrimaryResult",
			 "Columns":[{"ColumnName":"value","ColumnType":"long"},{"ColumnName":"dataAsOf","ColumnType":"datetime"}],
			 "Rows":[[1]]},
			{"FrameType":"DataSetCompletion","HasErrors":false}
		]`))
	}))
	defer server.Close()

	config := testConnectorConfig(t, server, externaltelemetry.PolicyConfig{MaxBytes: 4096}, externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone})
	config.Config = json.RawMessage(fmt.Sprintf(`{
		"cluster":%q,
		"database":"metrics",
		"watermarkColumn":"dataAsOf"
	}`, server.URL))
	connector, err := (Factory{}).Build(config, externaltelemetry.BuildOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = connector.Query(context.Background(), externaltelemetry.QueryRequest{
		Query:  "Metrics | take 1",
		Shape:  externaltelemetry.ShapeTimeSeries,
		Limits: externaltelemetry.QueryLimits{MaxBytes: 4096},
	})
	var queryErr *externaltelemetry.QueryError
	if !errors.As(err, &queryErr) || queryErr.Code != "invalid_response" {
		t.Fatalf("query error = %#v", err)
	}
}

func TestADXPreservesCredentialContextErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connector := &connector{
				cluster:  "http://127.0.0.1",
				database: "metrics",
				tokens: tokenSourceFunc(func(context.Context) (string, error) {
					return "", fmt.Errorf("credential acquisition: %w", test.err)
				}),
			}
			_, err := connector.Query(context.Background(), externaltelemetry.QueryRequest{
				Query:  "Metrics | take 1",
				Shape:  externaltelemetry.ShapeTable,
				Limits: externaltelemetry.QueryLimits{MaxBytes: 4096},
			})
			if !errors.Is(err, test.err) {
				t.Fatalf("query error = %v, want %v", err, test.err)
			}
			var queryErr *externaltelemetry.QueryError
			if errors.As(err, &queryErr) && queryErr.Code == "authentication_failed" {
				t.Fatalf("context error was misclassified as authentication failure: %#v", queryErr)
			}
		})
	}
}

func TestDecodeResponseRejectsV1Object(t *testing.T) {
	_, err := decodeResponse([]byte(`{
		"Tables":[{
			"TableName":"PrimaryResult",
			"Columns":[{"ColumnName":"requests","DataType":"long"}],
			"Rows":[]
		}]
	}`), map[string]string{"requests": "count"})
	if err == nil || !strings.Contains(err.Error(), "frame array") {
		t.Fatalf("v1 response error = %v", err)
	}
}

func TestADXNormalizesDecimalValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`[
			{"FrameType":"DataTable","TableKind":"PrimaryResult",
			 "Columns":[{"ColumnName":"availability","ColumnType":"decimal"}],
			 "Rows":[["99.999999999999999999"]]},
			{"FrameType":"DataSetCompletion","HasErrors":false}
		]`))
	}))
	defer server.Close()

	registry := configuredRegistry(
		t,
		server,
		externaltelemetry.PolicyConfig{MaxAttempts: 1, MaxRows: 1, MaxBytes: 4096},
		externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthNone},
		nil,
	)
	artifact, err := (&externaltelemetry.Host{Registry: registry}).Query(
		context.Background(),
		"adx",
		externaltelemetry.QueryRequest{Query: "print availability=decimal(99.999999999999999999)", Shape: externaltelemetry.ShapePoint},
	)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.State != externaltelemetry.DataPresent ||
		len(artifact.Columns) != 1 ||
		artifact.Columns[0].Type != externaltelemetry.TypeNumber ||
		len(artifact.Rows) != 1 ||
		len(artifact.Rows[0]) != 1 {
		t.Fatalf("artifact = %+v", artifact)
	}
	value, ok := artifact.Rows[0][0].(json.Number)
	if !ok || value.String() != "99.999999999999999999" {
		t.Fatalf("decimal value = %T(%v)", artifact.Rows[0][0], artifact.Rows[0][0])
	}
}

func TestWatermarkFromColumnUsesLatestTimestamp(t *testing.T) {
	result := externaltelemetry.SourceResult{
		Columns: []externaltelemetry.Column{
			{Name: "value", Type: externaltelemetry.TypeNumber},
			{Name: "dataAsOf", Type: externaltelemetry.TypeDateTime},
		},
		Rows: [][]any{
			{1.0, "2026-07-24T10:00:00Z"},
			{2.0, "2026-07-24T10:05:00Z"},
			{3.0, nil},
		},
	}
	watermark, err := watermarkFromColumn(result, "dataAsOf")
	if err != nil {
		t.Fatal(err)
	}
	if watermark == nil || watermark.Format(time.RFC3339) != "2026-07-24T10:05:00Z" {
		t.Fatalf("watermark = %v", watermark)
	}
	if _, err := watermarkFromColumn(result, "missing"); err == nil {
		t.Fatal("missing watermark column unexpectedly succeeded")
	}
}

func buildTestConnector(
	t *testing.T,
	server *httptest.Server,
	auth externaltelemetry.AuthConfig,
	registrar externaltelemetry.SecretRegistrar,
) externaltelemetry.Connector {
	t.Helper()
	config := testConnectorConfig(t, server, externaltelemetry.PolicyConfig{MaxBytes: 4096}, auth)
	connector, err := (Factory{}).Build(config, externaltelemetry.BuildOptions{
		HTTPClient: server.Client(),
		Registrar:  registrar,
	})
	if err != nil {
		t.Fatal(err)
	}
	return connector
}

func configuredRegistry(
	t *testing.T,
	server *httptest.Server,
	policy externaltelemetry.PolicyConfig,
	auth externaltelemetry.AuthConfig,
	registrar externaltelemetry.SecretRegistrar,
) *externaltelemetry.Registry {
	t.Helper()
	registry := externaltelemetry.NewRegistry()
	if err := registry.Register(Factory{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Configure(testConnectorConfig(t, server, policy, auth), server.Client(), registrar); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testConnectorConfig(
	t *testing.T,
	server *httptest.Server,
	policy externaltelemetry.PolicyConfig,
	auth externaltelemetry.AuthConfig,
) externaltelemetry.ConnectorConfig {
	t.Helper()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return externaltelemetry.ConnectorConfig{
		Name:    "adx",
		Kind:    Kind,
		Version: Version,
		Auth:    auth,
		Policy:  policy,
		Network: externaltelemetry.NetworkPolicy{
			AllowedHosts: []string{serverURL.Hostname()},
			AllowHTTP:    true,
		},
		Config: json.RawMessage(fmt.Sprintf(`{
			"cluster":%q,
			"database":"metrics",
			"columnUnits":{"latency":"ms"}
		}`, server.URL)),
	}
}

type recordingRegistrar struct {
	mu      sync.Mutex
	secrets []string
}

type tokenSourceFunc func(context.Context) (string, error)

func (f tokenSourceFunc) Token(ctx context.Context) (string, error) {
	return f(ctx)
}

func (r *recordingRegistrar) Register(secret []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, string(secret))
}

func (r *recordingRegistrar) contains(secret string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, candidate := range r.secrets {
		if candidate == secret {
			return true
		}
	}
	return false
}
