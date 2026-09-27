package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/microsoft/ApplicationInsights-Go/appinsights"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	azureMonitorConnectionStringMaxLength = 4096
	azureMonitorDefaultIngestionEndpoint  = "https://dc.services.visualstudio.com/"
)

type azureMonitorConnection struct {
	instrumentationKey string
	ingestionURL       string
}

// azureMonitorSpanExporter adapts the existing OTel span pipeline to the
// customer-owned Application Insights ingestion endpoint. The OTel batch span
// processor remains the producer-side bound; the SDK channel performs HTTP
// batching on its own worker, so workflow execution never waits on Azure.
type azureMonitorSpanExporter struct {
	client appinsights.TelemetryClient
	closed atomic.Bool
}

func newAzureMonitorSpanExporter(connectionString string, httpClient *http.Client) (*azureMonitorSpanExporter, error) {
	connection, err := parseAzureMonitorConnectionString(connectionString)
	if err != nil {
		return nil, fmt.Errorf("create Azure Monitor telemetry exporter: %w", err)
	}
	config := appinsights.NewTelemetryConfiguration(connection.instrumentationKey)
	config.EndpointUrl = connection.ingestionURL
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	config.Client = httpClient
	return &azureMonitorSpanExporter{client: appinsights.NewTelemetryClientFromConfig(config)}, nil
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

func (e *azureMonitorSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.closed.Load() {
		return errors.New("azure monitor telemetry exporter is shut down")
	}
	for _, span := range spans {
		if span == nil {
			continue
		}
		e.client.Track(azureMonitorSpan(span))
	}
	return nil
}

func (e *azureMonitorSpanExporter) Shutdown(ctx context.Context) error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	// The legacy Application Insights channel synchronously hands its close
	// request to the worker. Perform even that handoff off-thread so a worker
	// inside a bounded HTTP request cannot consume the caller's shutdown budget.
	closing := make(chan (<-chan struct{}), 1)
	go func() { closing <- e.client.Channel().Close() }()
	select {
	case done := <-closing:
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
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

var _ sdktrace.SpanExporter = (*azureMonitorSpanExporter)(nil)
