// Package adx implements the Azure Data Explorer KQL-over-REST connector.
package adx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/externaltelemetry"
)

const (
	// Kind is the stable ADX connector kind.
	Kind = "adx-kql-rest"
	// Version is the built-in ADX connector version.
	Version = "v1"

	// WindowStartParameter and WindowEndParameter are injected when a workflow
	// declares a requested time window.
	WindowStartParameter = "_goobers_window_start"
	// WindowEndParameter is the injected requested-window end.
	WindowEndParameter = "_goobers_window_end"
)

var controlCommandPattern = regexp.MustCompile(`(?m)(^|;)\s*\.`)

// Factory constructs ADX connectors.
type Factory struct{}

// Definition declares the ADX plugin contract.
func (Factory) Definition() externaltelemetry.Definition {
	return externaltelemetry.Definition{
		Kind:    Kind,
		Version: Version,
		ConfigurationSchema: json.RawMessage(
			`{"type":"object","required":["cluster","database"],"properties":{"cluster":{"type":"string","format":"uri"},"database":{"type":"string"},"columnUnits":{"type":"object","additionalProperties":{"type":"string"}},"watermarkColumn":{"type":"string"}},"additionalProperties":false}`,
		),
		AuthenticationModes: []string{
			externaltelemetry.AuthBearerToken,
			externaltelemetry.AuthAzureCLI,
			externaltelemetry.AuthWorkloadIdentity,
			externaltelemetry.AuthManagedIdentity,
			externaltelemetry.AuthDefaultAzure,
			externaltelemetry.AuthNone,
		},
		QueryLanguage: "kql",
		Shapes: []externaltelemetry.ResultShape{
			externaltelemetry.ShapePoint,
			externaltelemetry.ShapeTable,
			externaltelemetry.ShapeTimeSeries,
		},
	}
}

type adapterConfig struct {
	Cluster         string            `json:"cluster"`
	Database        string            `json:"database"`
	ColumnUnits     map[string]string `json:"columnUnits,omitempty"`
	WatermarkColumn string            `json:"watermarkColumn,omitempty"`
}

// ValidateConfig validates ADX-specific fields without acquiring credentials.
func (Factory) ValidateConfig(raw json.RawMessage) error {
	_, err := decodeConfig(raw)
	return err
}

// Build constructs one configured ADX connector.
func (Factory) Build(config externaltelemetry.ConnectorConfig, options externaltelemetry.BuildOptions) (externaltelemetry.Connector, error) {
	adapter, err := decodeConfig(config.Config)
	if err != nil {
		return nil, err
	}
	cluster, err := url.Parse(adapter.Cluster)
	if err != nil {
		return nil, fmt.Errorf("parse cluster: %w", err)
	}
	if !config.Network.Allows(cluster) {
		return nil, fmt.Errorf("cluster host %q is not permitted by connector network policy", cluster.Hostname())
	}
	mode := config.Auth.Mode
	if mode == "" {
		mode = externaltelemetry.AuthNone
	}
	if mode == externaltelemetry.AuthNone && (cluster.Scheme != "http" || !isLoopback(cluster.Hostname())) {
		return nil, errors.New("auth mode none is restricted to loopback HTTP fixtures")
	}
	tokenSource, err := newTokenSource(config.Name, config.Auth, strings.TrimRight(adapter.Cluster, "/")+"/.default", options.Registrar)
	if err != nil {
		return nil, err
	}
	if options.HTTPClient == nil {
		return nil, errors.New("host HTTP client is required")
	}
	return &connector{
		cluster:     strings.TrimRight(adapter.Cluster, "/"),
		database:    adapter.Database,
		columnUnits: adapter.ColumnUnits,
		watermark:   adapter.WatermarkColumn,
		client:      options.HTTPClient,
		tokens:      tokenSource,
	}, nil
}

func decodeConfig(raw json.RawMessage) (adapterConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config adapterConfig
	if err := decoder.Decode(&config); err != nil {
		return adapterConfig{}, fmt.Errorf("decode ADX config: %w", err)
	}
	cluster, err := url.Parse(config.Cluster)
	if err != nil || cluster.Hostname() == "" {
		return adapterConfig{}, errors.New("cluster must be an absolute HTTP or HTTPS URL")
	}
	if cluster.Scheme != "https" && cluster.Scheme != "http" {
		return adapterConfig{}, errors.New("cluster scheme must be HTTPS, or loopback HTTP for fixtures")
	}
	if cluster.User != nil || cluster.RawQuery != "" || cluster.Fragment != "" || (cluster.Path != "" && cluster.Path != "/") {
		return adapterConfig{}, errors.New("cluster must not contain credentials, query, fragment, or path")
	}
	if strings.TrimSpace(config.Database) == "" {
		return adapterConfig{}, errors.New("database is required")
	}
	for column, unit := range config.ColumnUnits {
		if strings.TrimSpace(column) == "" || strings.TrimSpace(unit) == "" {
			return adapterConfig{}, errors.New("columnUnits names and values must not be empty")
		}
	}
	if strings.TrimSpace(config.WatermarkColumn) != config.WatermarkColumn {
		return adapterConfig{}, errors.New("watermarkColumn must not contain leading or trailing whitespace")
	}
	return config, nil
}

type connector struct {
	cluster     string
	database    string
	columnUnits map[string]string
	watermark   string
	client      externaltelemetry.HTTPDoer
	tokens      tokenSource
}

func (c *connector) Descriptor() externaltelemetry.Descriptor {
	cluster, _ := url.Parse(c.cluster)
	return externaltelemetry.Descriptor{
		Kind:     Kind,
		Version:  Version,
		SourceID: cluster.Host + "/" + c.database,
	}
}

func (c *connector) Query(ctx context.Context, request externaltelemetry.QueryRequest) (externaltelemetry.SourceResult, error) {
	if controlCommandPattern.MatchString(request.Query) {
		return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError(
			"read_only_violation",
			"query",
			false,
			errors.New("ADX connector accepts data queries, not control commands"),
		)
	}
	payload, err := requestPayload(c.database, request)
	if err != nil {
		return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError("request_encoding", "query", false, err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cluster+"/v2/rest/query", bytes.NewReader(payload))
	if err != nil {
		return externaltelemetry.SourceResult{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	sum := sha256.Sum256(payload)
	httpRequest.Header.Set("x-ms-client-request-id", "goobers;"+hex.EncodeToString(sum[:16]))
	if c.tokens != nil {
		token, tokenErr := c.tokens.Token(ctx)
		if tokenErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return externaltelemetry.SourceResult{}, ctxErr
			}
			if errors.Is(tokenErr, context.Canceled) || errors.Is(tokenErr, context.DeadlineExceeded) {
				return externaltelemetry.SourceResult{}, tokenErr
			}
			return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError("authentication_failed", "authentication", false, tokenErr)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return externaltelemetry.SourceResult{}, ctx.Err()
		}
		return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError("transport_failed", "transport", true, fmt.Errorf("ADX query request: %w", err))
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return externaltelemetry.SourceResult{}, statusError(response.StatusCode)
	}

	bodyLimit := int64(request.Limits.MaxBytes)*2 + 64*1024
	body, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit+1))
	if err != nil {
		return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError("response_read_failed", "transport", true, fmt.Errorf("read ADX response: %w", err))
	}
	if int64(len(body)) > bodyLimit {
		return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError(
			"response_too_large",
			"limit",
			false,
			fmt.Errorf("ADX response exceeds %d-byte transport limit", bodyLimit),
		)
	}
	result, err := decodeResponse(body, c.columnUnits)
	if err != nil {
		return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError("invalid_response", "transport", false, err)
	}
	if c.watermark != "" {
		result.DataAsOf, err = watermarkFromColumn(result, c.watermark)
		if err != nil {
			return externaltelemetry.SourceResult{}, externaltelemetry.NewQueryError("invalid_watermark", "schema", false, err)
		}
	}
	if result.DataAsOf == nil {
		result.DataAsOf = responseDataAsOf(response.Header)
	}
	return result, nil
}

func requestPayload(database string, request externaltelemetry.QueryRequest) ([]byte, error) {
	body := struct {
		Database   string `json:"db"`
		Query      string `json:"csl"`
		Properties string `json:"properties,omitempty"`
	}{
		Database: database,
		Query:    request.Query,
	}
	parameters := make(map[string]any, len(request.Parameters)+2)
	for name, value := range request.Parameters {
		encoded, err := encodeParameter(value)
		if err != nil {
			return nil, fmt.Errorf("encode query parameter %q: %w", name, err)
		}
		parameters[name] = encoded
	}
	for _, reserved := range []string{WindowStartParameter, WindowEndParameter} {
		if _, exists := parameters[reserved]; exists {
			return nil, fmt.Errorf("query parameter %q is reserved by the ADX connector", reserved)
		}
	}
	if request.Window.Start != nil {
		parameters[WindowStartParameter] = "datetime(" + request.Window.Start.UTC().Format(time.RFC3339Nano) + ")"
	}
	if request.Window.End != nil {
		parameters[WindowEndParameter] = "datetime(" + request.Window.End.UTC().Format(time.RFC3339Nano) + ")"
	}
	properties, err := json.Marshal(struct {
		Options    map[string]bool `json:"Options"`
		Parameters map[string]any  `json:"Parameters,omitempty"`
	}{
		Options: map[string]bool{
			"request_callout_disabled":             true,
			"request_external_data_disabled":       true,
			"request_external_table_disabled":      true,
			"request_impersonation_disabled":       true,
			"request_readonly_hardline":            true,
			"request_remote_entities_disabled":     true,
			"request_sandboxed_execution_disabled": true,
		},
		Parameters: parameters,
	})
	if err != nil {
		return nil, fmt.Errorf("encode ADX request properties: %w", err)
	}
	body.Properties = string(properties)
	return json.Marshal(body)
}

func encodeParameter(value any) (any, error) {
	switch typed := value.(type) {
	case nil:
		return "dynamic(null)", nil
	case string:
		return typed, nil
	case json.Number:
		if _, err := typed.Int64(); err == nil {
			return typed, nil
		}
		if _, err := typed.Float64(); err != nil {
			return nil, fmt.Errorf("invalid numeric value %q", typed)
		}
		return "real(" + typed.String() + ")", nil
	case bool:
		return "bool(" + strconv.FormatBool(typed) + ")", nil
	case float32:
		return encodeReal(float64(typed), 32)
	case float64:
		return encodeReal(typed, 64)
	case int:
		return typed, nil
	case int8:
		return typed, nil
	case int16:
		return typed, nil
	case int32:
		return typed, nil
	case int64:
		return typed, nil
	case uint:
		return encodeUnsigned(uint64(typed))
	case uint8:
		return encodeUnsigned(uint64(typed))
	case uint16:
		return encodeUnsigned(uint64(typed))
	case uint32:
		return encodeUnsigned(uint64(typed))
	case uint64:
		return encodeUnsigned(typed)
	case map[string]any, []any, json.RawMessage:
		data, err := json.Marshal(typed)
		if err != nil {
			return nil, fmt.Errorf("encode dynamic value: %w", err)
		}
		return "dynamic(" + string(data) + ")", nil
	default:
		return nil, fmt.Errorf("unsupported value type %T", value)
	}
}

func encodeReal(value float64, bits int) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", errors.New("real value must be finite")
	}
	return "real(" + strconv.FormatFloat(value, 'g', -1, bits) + ")", nil
}

func encodeUnsigned(value uint64) (int64, error) {
	const maxInt64 = uint64(1<<63 - 1)
	if value > maxInt64 {
		return 0, fmt.Errorf("integer value %d exceeds ADX long range", value)
	}
	return int64(value), nil
}

func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return externaltelemetry.NewQueryError("authentication_failed", "authentication", false, fmt.Errorf("ADX query returned HTTP %d", status))
	case status == http.StatusTooManyRequests || status >= 500:
		return externaltelemetry.NewQueryError("service_unavailable", "transport", true, fmt.Errorf("ADX query returned HTTP %d", status))
	default:
		return externaltelemetry.NewQueryError("query_rejected", "query", false, fmt.Errorf("ADX query returned HTTP %d", status))
	}
}

type responseTable struct {
	TableName string
	TableKind string
	Columns   []responseColumn
	Rows      [][]any
}

type responseColumn struct {
	Name       string
	ColumnName string
	Type       string
	ColumnType string
	DataType   string
}

type responseFrame struct {
	FrameType string `json:"FrameType"`
	HasErrors bool   `json:"HasErrors"`
	Cancelled bool   `json:"Cancelled"`
	responseTable
}

func decodeResponse(data []byte, units map[string]string) (externaltelemetry.SourceResult, error) {
	var tables []responseTable
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return externaltelemetry.SourceResult{}, errors.New("ADX response is empty")
	}
	switch trimmed[0] {
	case '[':
		var frames []responseFrame
		if err := decodeJSON(data, &frames); err != nil {
			return externaltelemetry.SourceResult{}, fmt.Errorf("decode ADX v2 response: %w", err)
		}
		if len(frames) == 0 || frames[len(frames)-1].FrameType != "DataSetCompletion" {
			return externaltelemetry.SourceResult{}, errors.New("ADX v2 response is missing a final dataset completion frame")
		}
		for _, frame := range frames {
			switch frame.FrameType {
			case "DataTable":
				tables = append(tables, frame.responseTable)
			case "DataSetCompletion":
				if frame.HasErrors {
					return externaltelemetry.SourceResult{}, errors.New("ADX dataset completion reports query errors")
				}
				if frame.Cancelled {
					return externaltelemetry.SourceResult{}, errors.New("ADX dataset completion reports cancellation")
				}
			}
		}
	case '{':
		var response struct {
			Tables []responseTable `json:"Tables"`
		}
		if err := decodeJSON(data, &response); err != nil {
			return externaltelemetry.SourceResult{}, fmt.Errorf("decode ADX response: %w", err)
		}
		tables = response.Tables
	default:
		return externaltelemetry.SourceResult{}, errors.New("ADX response is not JSON")
	}

	primaryIndex := -1
	for i := range tables {
		if tables[i].TableKind == "PrimaryResult" || tables[i].TableName == "PrimaryResult" {
			if primaryIndex != -1 {
				return externaltelemetry.SourceResult{}, errors.New("ADX response has multiple primary result tables")
			}
			primaryIndex = i
		}
	}
	if primaryIndex == -1 && len(tables) == 1 {
		primaryIndex = 0
	}
	if primaryIndex == -1 {
		if len(tables) > 1 {
			return externaltelemetry.SourceResult{}, errors.New("ADX response has multiple tables and no primary result")
		}
		return externaltelemetry.SourceResult{}, errors.New("ADX response has no result table")
	}
	primary := &tables[primaryIndex]
	for rowIndex, row := range primary.Rows {
		if len(row) != len(primary.Columns) {
			return externaltelemetry.SourceResult{}, fmt.Errorf(
				"ADX result row %d has %d values for %d columns",
				rowIndex,
				len(row),
				len(primary.Columns),
			)
		}
	}
	result := externaltelemetry.SourceResult{
		Columns: make([]externaltelemetry.Column, 0, len(primary.Columns)),
		Rows:    make([][]any, len(primary.Rows)),
	}
	sourceTypes := make([]string, 0, len(primary.Columns))
	for _, column := range primary.Columns {
		name := column.ColumnName
		if name == "" {
			name = column.Name
		}
		sourceType := column.ColumnType
		if sourceType == "" {
			sourceType = column.DataType
		}
		if sourceType == "" {
			sourceType = column.Type
		}
		sourceTypes = append(sourceTypes, strings.ToLower(sourceType))
		normalized, err := normalizeColumnType(sourceType)
		if err != nil {
			return externaltelemetry.SourceResult{}, fmt.Errorf("column %q: %w", name, err)
		}
		result.Columns = append(result.Columns, externaltelemetry.Column{
			Name: name,
			Type: normalized,
			Unit: units[name],
		})
	}
	for rowIndex, row := range primary.Rows {
		normalized := append([]any(nil), row...)
		for columnIndex, sourceType := range sourceTypes {
			if sourceType != "decimal" || normalized[columnIndex] == nil {
				continue
			}
			value, err := normalizeDecimal(normalized[columnIndex])
			if err != nil {
				return externaltelemetry.SourceResult{}, fmt.Errorf(
					"row %d column %q: %w",
					rowIndex,
					result.Columns[columnIndex].Name,
					err,
				)
			}
			normalized[columnIndex] = value
		}
		result.Rows[rowIndex] = normalized
	}
	return result, nil
}

func normalizeDecimal(value any) (any, error) {
	switch typed := value.(type) {
	case string:
		decoder := json.NewDecoder(strings.NewReader(typed))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("decode decimal value %q: %w", typed, err)
		}
		number, ok := decoded.(json.Number)
		if !ok {
			return nil, fmt.Errorf("decimal value %q is not a number", typed)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decimal value %q has trailing data", typed)
		}
		return number, nil
	case json.Number, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return typed, nil
	default:
		return nil, fmt.Errorf("decimal value has unexpected type %T", value)
	}
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func normalizeColumnType(value string) (externaltelemetry.ColumnType, error) {
	switch strings.ToLower(value) {
	case "string", "guid":
		return externaltelemetry.TypeString, nil
	case "int", "long", "int32", "int64":
		return externaltelemetry.TypeInteger, nil
	case "real", "decimal", "double", "float":
		return externaltelemetry.TypeNumber, nil
	case "bool", "boolean":
		return externaltelemetry.TypeBoolean, nil
	case "datetime", "date":
		return externaltelemetry.TypeDateTime, nil
	case "timespan":
		return externaltelemetry.TypeDuration, nil
	case "dynamic":
		return externaltelemetry.TypeJSON, nil
	default:
		return "", fmt.Errorf("unsupported ADX column type %q", value)
	}
}

func responseDataAsOf(headers http.Header) *time.Time {
	for _, name := range []string{"x-ms-kusto-data-as-of", "x-ms-data-as-of", "Last-Modified"} {
		if value := headers.Get(name); value != "" {
			if parsed, err := http.ParseTime(value); err == nil {
				parsed = parsed.UTC()
				return &parsed
			}

			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				parsed = parsed.UTC()
				return &parsed
			}
		}
	}
	return nil
}

func watermarkFromColumn(result externaltelemetry.SourceResult, name string) (*time.Time, error) {
	columnIndex := -1
	for i, column := range result.Columns {
		if column.Name != name {
			continue
		}
		if column.Type != externaltelemetry.TypeDateTime {
			return nil, fmt.Errorf("watermark column %q must have datetime type", name)
		}
		columnIndex = i
		break
	}
	if columnIndex == -1 {
		return nil, fmt.Errorf("watermark column %q is absent from the result", name)
	}
	var latest *time.Time
	for rowIndex, row := range result.Rows {
		if columnIndex >= len(row) {
			return nil, fmt.Errorf("watermark column %q row %d is missing from a short row", name, rowIndex)
		}
		if row[columnIndex] == nil {
			continue
		}
		value, ok := row[columnIndex].(string)
		if !ok {
			return nil, fmt.Errorf("watermark column %q row %d is not a timestamp string", name, rowIndex)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("watermark column %q row %d: %w", name, rowIndex, err)
		}
		parsed = parsed.UTC()
		if latest == nil || parsed.After(*latest) {
			copy := parsed
			latest = &copy
		}
	}
	return latest, nil
}

type tokenSource interface {
	Token(context.Context) (string, error)
}

type resolvingTokenSource struct {
	resolver  credentials.Resolver
	ref       string
	registrar externaltelemetry.SecretRegistrar
}

func (s resolvingTokenSource) Token(ctx context.Context) (string, error) {
	token, err := s.resolver.Resolve(ctx, s.ref)
	if err != nil {
		return "", err
	}
	if s.registrar != nil {
		s.registrar.Register([]byte(token))
	}
	return token, nil
}

type azureCredential interface {
	GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)
}

type azureTokenSource struct {
	credential azureCredential
	scope      string
	registrar  externaltelemetry.SecretRegistrar
}

func (s azureTokenSource) Token(ctx context.Context) (string, error) {
	token, err := s.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{s.scope}})
	if err != nil {
		return "", fmt.Errorf("acquire Azure Data Explorer token: %w", err)
	}
	if strings.TrimSpace(token.Token) == "" {
		return "", errors.New("azure credential returned an empty token")
	}
	if s.registrar != nil {
		s.registrar.Register([]byte(token.Token))
	}
	return token.Token, nil
}

func newTokenSource(name string, auth externaltelemetry.AuthConfig, scope string, registrar externaltelemetry.SecretRegistrar) (tokenSource, error) {
	mode := auth.Mode
	if mode == "" {
		mode = externaltelemetry.AuthNone
	}
	switch mode {
	case externaltelemetry.AuthNone:
		return nil, nil
	case externaltelemetry.AuthBearerToken:
		const prefix = "external-telemetry:"
		ref := prefix + name
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{
			Name: ref,
			Env:  auth.Token.Env,
			File: auth.Token.File,
		}})
		if err != nil {
			return nil, fmt.Errorf("configure bearer token: %w", err)
		}
		return resolvingTokenSource{resolver: resolver, ref: ref, registrar: registrar}, nil
	case externaltelemetry.AuthAzureCLI:
		credential, err := azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{TenantID: auth.Tenant})
		if err != nil {
			return nil, fmt.Errorf("create Azure CLI credential: %w", err)
		}
		return azureTokenSource{credential: credential, scope: scope, registrar: registrar}, nil
	case externaltelemetry.AuthWorkloadIdentity:
		credential, err := azidentity.NewWorkloadIdentityCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("create workload identity credential: %w", err)
		}
		return azureTokenSource{credential: credential, scope: scope, registrar: registrar}, nil
	case externaltelemetry.AuthManagedIdentity:
		options := &azidentity.ManagedIdentityCredentialOptions{}
		if auth.ClientID != "" {
			options.ID = azidentity.ClientID(auth.ClientID)
		}
		credential, err := azidentity.NewManagedIdentityCredential(options)
		if err != nil {
			return nil, fmt.Errorf("create managed identity credential: %w", err)
		}
		return azureTokenSource{credential: credential, scope: scope, registrar: registrar}, nil
	case externaltelemetry.AuthDefaultAzure:
		credential, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{TenantID: auth.Tenant})
		if err != nil {
			return nil, fmt.Errorf("create default Azure credential: %w", err)
		}
		return azureTokenSource{credential: credential, scope: scope, registrar: registrar}, nil
	default:
		return nil, fmt.Errorf("unsupported ADX auth mode %q", mode)
	}
}

func isLoopback(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}
