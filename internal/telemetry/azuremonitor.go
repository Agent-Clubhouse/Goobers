package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/ApplicationInsights-Go/appinsights"
	"github.com/microsoft/ApplicationInsights-Go/appinsights/contracts"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

const (
	azureMonitorConnectionStringMaxLength = 4096
	azureMonitorDefaultIngestionEndpoint  = "https://dc.services.visualstudio.com/"
)

// AzureMonitorConnectivityResult identifies one secret-safe ingestion probe.
// RecordID can be queried in Application Insights without exposing the
// connection string or any host/user identity.
type AzureMonitorConnectivityResult struct {
	Schema     string    `json:"schema"`
	Accepted   bool      `json:"accepted"`
	RecordID   string    `json:"recordId"`
	ObservedAt time.Time `json:"observedAt"`
}

// TestAzureMonitorConnectivity sends one fixed, low-cardinality trace directly
// to the configured Application Insights ingestion endpoint. It deliberately
// bypasses replay: success means Azure acknowledged this request now.
func TestAzureMonitorConnectivity(ctx context.Context, connectionString string, httpClient *http.Client) (AzureMonitorConnectivityResult, error) {
	observedAt := time.Now().UTC()
	client, err := newAzureMonitorClient(connectionString, httpClient, false, azureReplayConfig{})
	if err != nil {
		return AzureMonitorConnectivityResult{}, err
	}
	item := appinsights.NewTraceTelemetry("goobers.telemetry.connectivity", contracts.Information)
	item.Timestamp = observedAt
	item.Properties["goobers.telemetry.kind"] = "connectivity_test"
	item.Properties["goobers.telemetry.schema"] = "goobers.dev/telemetry/connectivity/v1"
	if err := client.export(ctx, []appinsights.Telemetry{item}); err != nil {
		return AzureMonitorConnectivityResult{}, err
	}
	return AzureMonitorConnectivityResult{
		Schema: "goobers.dev/telemetry/connectivity/v1", Accepted: true,
		RecordID: item.Properties["goobers.telemetry.record_id"], ObservedAt: observedAt,
	}, nil
}

type azureMonitorConnection struct {
	instrumentationKey string
	ingestionURL       string
}

type azureMonitorClient struct {
	instrumentationKey string
	nameKey            string
	ingestionURL       string
	httpClient         *http.Client
	defaultTags        contracts.ContextTags
	replay             *azureReplaySpool
}

// azureMonitorSpanExporter adapts the existing OTel span pipeline to the
// customer-owned Application Insights ingestion endpoint. The OTel batch span
// processor remains the producer-side bound and performs this HTTP export on
// its worker, so workflow execution never waits on Azure.
type azureMonitorSpanExporter struct {
	client *azureMonitorClient
	mu     sync.RWMutex
	closed atomic.Bool
}

func newAzureMonitorSpanExporter(connectionString string, httpClient *http.Client, includeHostIdentity bool, replay azureReplayConfig) (*azureMonitorSpanExporter, error) {
	client, err := newAzureMonitorClient(connectionString, httpClient, includeHostIdentity, replay)
	if err != nil {
		return nil, fmt.Errorf("create Azure Monitor telemetry exporter: %w", err)
	}
	return &azureMonitorSpanExporter{client: client}, nil
}

func newAzureMonitorClient(connectionString string, httpClient *http.Client, includeHostIdentity bool, replay azureReplayConfig) (*azureMonitorClient, error) {
	connection, err := parseAzureMonitorConnectionString(connectionString)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	tags := contracts.ContextTags{
		contracts.DeviceOSVersion:    runtime.GOOS,
		contracts.InternalSdkVersion: "goobers:" + appinsights.Version,
	}
	if includeHostIdentity {
		if hostname, err := os.Hostname(); err == nil {
			tags[contracts.DeviceId] = hostname
			tags[contracts.CloudRoleInstance] = hostname
		}
	}
	client := &azureMonitorClient{
		instrumentationKey: connection.instrumentationKey,
		nameKey:            strings.ReplaceAll(connection.instrumentationKey, "-", ""),
		ingestionURL:       connection.ingestionURL,
		httpClient:         httpClient,
		defaultTags:        tags,
	}
	spool, err := newAzureReplaySpool(replay, client.sendPayload)
	if err != nil {
		return nil, err
	}
	client.replay = spool
	return client, nil
}

type azureMonitorIngestionResponse struct {
	ItemsReceived int `json:"itemsReceived"`
	ItemsAccepted int `json:"itemsAccepted"`
}

func (c *azureMonitorClient) export(ctx context.Context, items []appinsights.Telemetry) error {
	if len(items) == 0 {
		return nil
	}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	for _, item := range items {
		if item == nil {
			continue
		}
		if properties := item.GetProperties(); properties != nil && properties["goobers.telemetry.record_id"] == "" {
			id, err := azureReplayID()
			if err != nil {
				return err
			}
			properties["goobers.telemetry.record_id"] = id
		}
		if err := encoder.Encode(c.envelope(item)); err != nil {
			return fmt.Errorf("encode Azure Monitor telemetry: %w", err)
		}
	}
	if c.replay != nil {
		return c.replay.submit(ctx, raw.Bytes())
	}
	return c.sendPayload(ctx, raw.Bytes())
}

func (c *azureMonitorClient) sendPayload(ctx context.Context, raw []byte) error {
	if err := c.sendPayloadRequest(ctx, raw); err != nil {
		return &azureMonitorDeliveryError{cause: err}
	}
	return nil
}

func (c *azureMonitorClient) sendPayloadRequest(ctx context.Context, raw []byte) error {
	var compressed bytes.Buffer
	if err := azureMonitorCompression.compress(&compressed, raw); err != nil {
		return fmt.Errorf("compress Azure Monitor telemetry: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ingestionURL, &compressed)
	if err != nil {
		return fmt.Errorf("create Azure Monitor ingestion request: %w", err)
	}
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Content-Type", "application/x-json-stream")
	request.Header.Set("Accept-Encoding", "gzip, deflate")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send Azure Monitor telemetry: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	const responseLimit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return fmt.Errorf("read Azure Monitor ingestion response: %w", err)
	}
	if len(body) > responseLimit {
		return errors.New("the Azure Monitor ingestion response exceeds 1 MiB")
	}
	if response.StatusCode == http.StatusOK {
		return nil
	}
	if response.StatusCode == http.StatusPartialContent {
		var result azureMonitorIngestionResponse
		itemCount := len(bytesLines(raw))
		if err := json.Unmarshal(body, &result); err == nil && result.ItemsReceived == itemCount && result.ItemsAccepted == itemCount {
			return nil
		}
		return fmt.Errorf("the Azure Monitor destination partially rejected telemetry (HTTP %d)", response.StatusCode)
	}
	return fmt.Errorf("the Azure Monitor destination rejected telemetry (HTTP %d)", response.StatusCode)
}

func (c *azureMonitorClient) envelope(item appinsights.Telemetry) *contracts.Envelope {
	// The OTel process-instance tag can override Azure's default hostname role
	// tag. Preserve a queryable machine name independently, but only when this
	// Azure destination explicitly consented to host identity at construction.
	if host := c.defaultTags[contracts.DeviceId]; host != "" && item.GetProperties() != nil {
		item.GetProperties()["host.name"] = host
	}
	dataContract := item.TelemetryData()
	warnings := dataContract.Sanitize()
	if len(warnings) != 0 && item.GetProperties() != nil {
		item.GetProperties()["goobers.azure_monitor.truncated"] = "true"
	}
	data := contracts.NewData()
	data.BaseType = dataContract.BaseType()
	data.BaseData = dataContract
	envelope := contracts.NewEnvelope()
	envelope.Name = dataContract.EnvelopeName(c.nameKey)
	envelope.Data = data
	envelope.IKey = c.instrumentationKey
	timestamp := item.Time()
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	envelope.Time = timestamp.UTC().Format("2006-01-02T15:04:05.999999Z")
	envelope.Tags = make(contracts.ContextTags, len(c.defaultTags)+len(item.ContextTags()))
	for key, value := range c.defaultTags {
		envelope.Tags[key] = value
	}
	for key, value := range item.ContextTags() {
		envelope.Tags[key] = value
	}
	_ = contracts.SanitizeTags(envelope.Tags)
	_ = envelope.Sanitize()
	return envelope
}

func parseAzureMonitorConnectionString(raw string) (azureMonitorConnection, error) {
	if raw == "" {
		return azureMonitorConnection{}, errors.New("connection string is empty")
	}
	if len(raw) > azureMonitorConnectionStringMaxLength {
		return azureMonitorConnection{}, fmt.Errorf("connection string exceeds %d bytes", azureMonitorConnectionStringMaxLength)
	}
	values := make(map[string]string)
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return azureMonitorConnection{}, errors.New("connection string contains an invalid field")
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		if _, exists := values[key]; exists {
			return azureMonitorConnection{}, fmt.Errorf("connection string field %q is repeated", strings.TrimSpace(parts[0]))
		}
		values[key] = strings.TrimSpace(parts[1])
	}
	instrumentationKey := values["instrumentationkey"]
	if instrumentationKey == "" {
		return azureMonitorConnection{}, errors.New("connection string requires InstrumentationKey")
	}
	endpoint := values["ingestionendpoint"]
	if endpoint == "" {
		endpoint = azureMonitorDefaultIngestionEndpoint
	}
	ingestionURL, err := azureMonitorIngestionURL(endpoint)
	if err != nil {
		return azureMonitorConnection{}, err
	}
	return azureMonitorConnection{instrumentationKey: instrumentationKey, ingestionURL: ingestionURL}, nil
}

func azureMonitorIngestionURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("connection string has an invalid IngestionEndpoint")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		loopback := strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
		if u.Scheme != "http" || !loopback {
			return "", errors.New("IngestionEndpoint must use HTTPS (HTTP is accepted only on loopback for tests)")
		}
	}
	u.Path = path.Join(u.Path, "/v2.1/track")
	return u.String(), nil
}

func (e *azureMonitorSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed.Load() {
		return errors.New("azure monitor telemetry exporter is shut down")
	}
	items := make([]appinsights.Telemetry, 0, len(spans))
	for _, span := range spans {
		if span == nil {
			continue
		}
		items = append(items, azureMonitorSpan(span))
	}
	return e.client.export(ctx, items)
}

func (e *azureMonitorSpanExporter) Shutdown(ctx context.Context) error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.client.replay.close(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func azureMonitorSpan(span sdktrace.ReadOnlySpan) appinsights.Telemetry {
	success := span.Status().Code != codes.Error
	item := appinsights.NewRemoteDependencyTelemetry(span.Name(), "InProc", "goobers", success)
	item.Id = span.SpanContext().SpanID().String()
	item.ResultCode = span.Status().Code.String()
	item.Data = span.Name()
	item.MarkTime(span.StartTime(), span.EndTime())
	item.Tags.Operation().SetId(span.SpanContext().TraceID().String())
	item.Tags.Operation().SetName(span.Name())
	if parent := span.Parent().SpanID(); parent.IsValid() {
		item.Tags.Operation().SetParentId(parent.String())
	}
	for _, attr := range span.Resource().Attributes() {
		azureMonitorAttribute(item.Properties, item.Measurements, attr)
	}
	for _, attr := range span.Attributes() {
		azureMonitorAttribute(item.Properties, item.Measurements, attr)
	}
	if scope := span.InstrumentationScope(); scope.Name != "" {
		item.Properties["otel.scope.name"] = scope.Name
		if scope.Version != "" {
			item.Properties["otel.scope.version"] = scope.Version
		}
	}
	if description := span.Status().Description; description != "" {
		item.Properties["otel.status_description"] = description
	}
	if value := item.Properties["service.name"]; value != "" {
		item.Tags.Cloud().SetRole(value)
	}
	if value := item.Properties["service.instance.id"]; value != "" {
		item.Tags.Cloud().SetRoleInstance(value)
	}
	if value := item.Properties["service.version"]; value != "" {
		item.Tags.Application().SetVer(value)
	}
	return item
}

func azureMonitorAttribute(properties map[string]string, measurements map[string]float64, attr attribute.KeyValue) {
	key := string(attr.Key)
	switch attr.Value.Type() {
	case attribute.INT64:
		measurements[key] = float64(attr.Value.AsInt64())
	case attribute.FLOAT64:
		measurements[key] = attr.Value.AsFloat64()
	case attribute.BOOL:
		properties[key] = strconv.FormatBool(attr.Value.AsBool())
	case attribute.STRING:
		properties[key] = attr.Value.AsString()
	default:
		properties[key] = attr.Value.String()
	}
}

// azureMonitorLogExporter adapts both committed journal records and the
// whitelisted diagnostic stream to Application Insights TraceTelemetry. The
// callers retain their existing bounded queues and invoke this exporter only
// from background workers. With replay enabled, Export reports durable local
// admission and replay stats report remote delivery; without replay, Export
// reports HTTP ingestion. Neither puts network work on a journal/workflow path.
type azureMonitorLogExporter struct {
	client *azureMonitorClient
	mu     sync.RWMutex
	closed atomic.Bool
}

func (e *azureMonitorLogExporter) ReplayStats() AzureReplayStats {
	if e == nil || e.client == nil {
		return AzureReplayStats{}
	}
	return e.client.replay.stats()
}

func (e *azureMonitorLogExporter) setReplayLossSource(sample func() replayLossCounters) {
	if e != nil && e.client != nil && e.client.replay != nil {
		setReplayLossSource(&e.client.replay.lossSource, sample)
	}
}

func newAzureMonitorLogExporter(connectionString string, httpClient *http.Client, includeHostIdentity bool, replay azureReplayConfig) (*azureMonitorLogExporter, error) {
	client, err := newAzureMonitorClient(connectionString, httpClient, includeHostIdentity, replay)
	if err != nil {
		return nil, fmt.Errorf("create Azure Monitor log exporter: %w", err)
	}
	return &azureMonitorLogExporter{client: client}, nil
}

func (e *azureMonitorLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed.Load() {
		return sdklog.ErrExporterShutdown
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	items := make([]appinsights.Telemetry, 0, len(records))
	for i := range records {
		items = append(items, azureMonitorLog(&records[i]))
	}
	return e.client.export(ctx, items)
}

func (e *azureMonitorLogExporter) ForceFlush(ctx context.Context) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed.Load() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (e *azureMonitorLogExporter) Shutdown(ctx context.Context) error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.client.replay.close(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func azureMonitorLog(record *sdklog.Record) *appinsights.TraceTelemetry {
	body := record.Body()
	message := body.String()
	if body.Type() == attribute.STRING {
		message = body.AsString()
	}
	item := appinsights.NewTraceTelemetry(message, azureMonitorSeverity(record.Severity()))
	if timestamp := record.Timestamp(); !timestamp.IsZero() {
		item.Timestamp = timestamp
	}
	if res := record.Resource(); res != nil {
		for _, attr := range res.Attributes() {
			item.Properties[string(attr.Key)] = attr.Value.String()
		}
	}
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		item.Properties[string(attr.Key)] = attr.Value.String()
		return true
	})
	if journalID, seq := item.Properties["goobers.journal.id"], item.Properties["goobers.journal.seq"]; journalID != "" && seq != "" {
		// Stable across cursor replay and an ambiguous ingestion acknowledgement.
		// Hash structured identity, not the body or any human-authored label.
		identity, _ := json.Marshal([]string{item.Properties["goobers.instance.id"], item.Properties["goobers.journal.kind"], journalID, seq})
		digest := sha256.Sum256(identity)
		item.Properties["goobers.telemetry.record_id"] = hex.EncodeToString(digest[:])
	}
	if scope := record.InstrumentationScope(); scope.Name != "" {
		item.Properties["otel.scope.name"] = scope.Name
		if scope.Version != "" {
			item.Properties["otel.scope.version"] = scope.Version
		}
	}
	if traceID := record.TraceID(); traceID.IsValid() {
		item.Tags.Operation().SetId(traceID.String())
	}
	if spanID := record.SpanID(); spanID.IsValid() {
		item.Tags.Operation().SetParentId(spanID.String())
	}
	if value := item.Properties["service.name"]; value != "" {
		item.Tags.Cloud().SetRole(value)
	}
	if value := item.Properties["service.instance.id"]; value != "" {
		item.Tags.Cloud().SetRoleInstance(value)
	}
	if value := item.Properties["service.version"]; value != "" {
		item.Tags.Application().SetVer(value)
	}
	return item
}

func azureMonitorSeverity(severity otellog.Severity) contracts.SeverityLevel {
	switch {
	case severity >= otellog.SeverityFatal:
		return contracts.Critical
	case severity >= otellog.SeverityError:
		return contracts.Error
	case severity >= otellog.SeverityWarn:
		return contracts.Warning
	case severity >= otellog.SeverityInfo:
		return contracts.Information
	default:
		return contracts.Verbose
	}
}

func (e *azureMonitorLogExporter) exportDiagnosticRecords(ctx context.Context, resource *resourcepb.Resource, records []*logpb.LogRecord) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed.Load() {
		return sdklog.ErrExporterShutdown
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	items := make([]appinsights.Telemetry, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		item := appinsights.NewTraceTelemetry(protoLogValue(record.Body), azureMonitorProtoSeverity(record.SeverityNumber))
		if record.TimeUnixNano != 0 {
			item.Timestamp = time.Unix(0, int64(record.TimeUnixNano))
		}
		if resource != nil {
			for _, attr := range resource.Attributes {
				item.Properties[attr.Key] = protoLogValue(attr.Value)
			}
		}
		for _, attr := range record.Attributes {
			item.Properties[attr.Key] = protoLogValue(attr.Value)
		}
		item.Properties["otel.scope.name"] = "goobers.diagnostics"
		item.Properties["otel.scope.version"] = "1"
		if value := item.Properties["service.name"]; value != "" {
			item.Tags.Cloud().SetRole(value)
		}
		if value := item.Properties["service.instance.id"]; value != "" {
			item.Tags.Cloud().SetRoleInstance(value)
		}
		if value := item.Properties["service.version"]; value != "" {
			item.Tags.Application().SetVer(value)
		}
		items = append(items, item)
	}
	return e.client.export(ctx, items)
}

func protoLogValue(value *commonpb.AnyValue) string {
	if value == nil {
		return ""
	}
	switch typed := value.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return typed.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(typed.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(typed.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(typed.DoubleValue, 'g', -1, 64)
	default:
		return ""
	}
}

func azureMonitorProtoSeverity(severity logpb.SeverityNumber) contracts.SeverityLevel {
	switch {
	case severity >= logpb.SeverityNumber_SEVERITY_NUMBER_FATAL:
		return contracts.Critical
	case severity >= logpb.SeverityNumber_SEVERITY_NUMBER_ERROR:
		return contracts.Error
	case severity >= logpb.SeverityNumber_SEVERITY_NUMBER_WARN:
		return contracts.Warning
	case severity >= logpb.SeverityNumber_SEVERITY_NUMBER_INFO:
		return contracts.Information
	default:
		return contracts.Verbose
	}
}

var _ sdktrace.SpanExporter = (*azureMonitorSpanExporter)(nil)
var _ sdklog.Exporter = (*azureMonitorLogExporter)(nil)
