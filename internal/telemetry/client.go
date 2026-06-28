// Package telemetry exposes the small span-helper API used by the scheduler,
// workflow engine, and goober runtime. Callers describe Goobers domain events;
// this package owns the OpenTelemetry setup and attribute mapping.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// ScopeName is the instrumentation scope name used for Goobers telemetry.
	ScopeName = "github.com/goobers/goobers/internal/telemetry"

	// ExporterOTLP sends spans to an OTLP gRPC collector.
	ExporterOTLP ExporterKind = "otlp"
	// ExporterStdout writes spans to stdout for local development.
	ExporterStdout ExporterKind = "stdout"
)

// ExporterKind selects the built-in span exporter.
type ExporterKind string

// Config controls tracer/meter setup and exporter selection.
type Config struct {
	ServiceName        string
	ServiceVersion     string
	Environment        string
	Exporter           ExporterKind
	OTLPEndpoint       string
	OTLPInsecure       bool
	Stdout             io.Writer
	SpanExporter       sdktrace.SpanExporter
	ResourceAttributes []attribute.KeyValue
	Batch              bool
}

// Client owns the OTel tracer and meter providers for a Goobers process.
type Client struct {
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *metric.MeterProvider
	tracer         trace.Tracer
}

// New configures OpenTelemetry tracing and metrics for a Goobers process.
func New(ctx context.Context, cfg Config) (*Client, error) {
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "goobers"
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(resourceAttrs(serviceName, cfg)...),
	)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	exporter, err := spanExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(runIDGenerator{}),
	}
	if cfg.Batch {
		options = append(options, sdktrace.WithBatcher(exporter))
	} else {
		options = append(options, sdktrace.WithSyncer(exporter))
	}

	tracerProvider := sdktrace.NewTracerProvider(options...)
	meterProvider := metric.NewMeterProvider(metric.WithResource(res))
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)

	return &Client{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		tracer:         tracerProvider.Tracer(ScopeName),
	}, nil
}

// NewRunID returns a valid OpenTelemetry trace id for use as a Goobers run id.
func NewRunID() (string, error) {
	var b [16]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate run trace id: %w", err)
		}
		id, err := trace.TraceIDFromHex(hex.EncodeToString(b[:]))
		if err == nil && id.IsValid() {
			return id.String(), nil
		}
	}
}

// StartRun starts the root span for a workflow run.
func (c *Client) StartRun(ctx context.Context, attrs RunAttributes) (context.Context, Span, error) {
	if err := validateCommon(attrs.Gaggle, attrs.WorkflowID, attrs.RunID); err != nil {
		return ctx, Span{}, err
	}
	traceID, err := parseTraceID(attrs.RunID)
	if err != nil {
		return ctx, Span{}, err
	}
	ctx = contextWithRequestedTraceID(ctx, traceID)
	ctx, span := c.tracer.Start(ctx, runSpanName(attrs.WorkflowID),
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(runAttributeSet(attrs)...),
	)
	return ctx, Span{span: span}, nil
}

// StartTask starts a task span under the current run context.
func (c *Client) StartTask(ctx context.Context, attrs TaskAttributes) (context.Context, Span, error) {
	if err := validateCommon(attrs.Gaggle, attrs.WorkflowID, attrs.RunID); err != nil {
		return ctx, Span{}, err
	}
	if attrs.TaskID == "" {
		return ctx, Span{}, errors.New("telemetry task span requires task id")
	}
	var err error
	ctx, err = contextWithRunTraceID(ctx, attrs.RunID)
	if err != nil {
		return ctx, Span{}, err
	}
	ctx, span := c.tracer.Start(ctx, taskSpanName(attrs.TaskID),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(taskAttributeSet(attrs)...),
	)
	return ctx, Span{span: span}, nil
}

// StartGate starts a gate evaluation span under the current run context.
func (c *Client) StartGate(ctx context.Context, attrs GateAttributes) (context.Context, Span, error) {
	if err := validateCommon(attrs.Gaggle, attrs.WorkflowID, attrs.RunID); err != nil {
		return ctx, Span{}, err
	}
	if attrs.GateID == "" {
		return ctx, Span{}, errors.New("telemetry gate span requires gate id")
	}
	var err error
	ctx, err = contextWithRunTraceID(ctx, attrs.RunID)
	if err != nil {
		return ctx, Span{}, err
	}
	ctx, span := c.tracer.Start(ctx, gateSpanName(attrs.GateID),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(gateAttributeSet(attrs)...),
	)
	return ctx, Span{span: span}, nil
}

// StartSchedulerSpan starts a scheduler decision span.
func (c *Client) StartSchedulerSpan(ctx context.Context, attrs SchedulerAttributes) (context.Context, Span, error) {
	if attrs.Gaggle == "" {
		return ctx, Span{}, errors.New("telemetry scheduler span requires gaggle")
	}
	if attrs.WorkflowID == "" {
		return ctx, Span{}, errors.New("telemetry scheduler span requires workflow id")
	}
	if attrs.Action == "" {
		return ctx, Span{}, errors.New("telemetry scheduler span requires action")
	}
	if attrs.RunID != "" {
		var err error
		ctx, err = contextWithRunTraceID(ctx, attrs.RunID)
		if err != nil {
			return ctx, Span{}, err
		}
	}
	ctx, span := c.tracer.Start(ctx, schedulerSpanName(attrs.Action),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(schedulerAttributeSet(attrs)...),
	)
	return ctx, Span{span: span}, nil
}

// Flush forces pending telemetry through configured providers.
func (c *Client) Flush(ctx context.Context) error {
	if err := c.tracerProvider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("flush telemetry traces: %w", err)
	}
	if err := c.meterProvider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("flush telemetry metrics: %w", err)
	}
	return nil
}

// Shutdown flushes and shuts down telemetry providers.
func (c *Client) Shutdown(ctx context.Context) error {
	var errs []error
	if err := c.tracerProvider.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown telemetry traces: %w", err))
	}
	if err := c.meterProvider.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown telemetry metrics: %w", err))
	}
	return errors.Join(errs...)
}

func spanExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	if cfg.SpanExporter != nil {
		return cfg.SpanExporter, nil
	}

	switch cfg.Exporter {
	case "", ExporterStdout:
		opts := []stdouttrace.Option{stdouttrace.WithPrettyPrint()}
		if cfg.Stdout != nil {
			opts = append(opts, stdouttrace.WithWriter(cfg.Stdout))
		} else {
			opts = append(opts, stdouttrace.WithWriter(os.Stdout))
		}
		exporter, err := stdouttrace.New(opts...)
		if err != nil {
			return nil, fmt.Errorf("create stdout telemetry exporter: %w", err)
		}
		return exporter, nil
	case ExporterOTLP:
		opts := []otlptracegrpc.Option{}
		if cfg.OTLPEndpoint != "" {
			if strings.Contains(cfg.OTLPEndpoint, "://") {
				opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.OTLPEndpoint))
			} else {
				opts = append(opts, otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint))
			}
		}
		if cfg.OTLPInsecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp telemetry exporter: %w", err)
		}
		return exporter, nil
	default:
		return nil, fmt.Errorf("unsupported telemetry exporter %q", cfg.Exporter)
	}
}

func resourceAttrs(serviceName string, cfg Config) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", serviceName),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment", cfg.Environment))
	}
	return append(attrs, cfg.ResourceAttributes...)
}

func validateCommon(gaggle, workflowID, runID string) error {
	if gaggle == "" {
		return errors.New("telemetry span requires gaggle")
	}
	if workflowID == "" {
		return errors.New("telemetry span requires workflow id")
	}
	if runID == "" {
		return errors.New("telemetry span requires run id")
	}
	return nil
}
