package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

// OneCollectorOptions is resolved only after the external profile's explicit opt-in.
type OneCollectorOptions struct {
	Profile   onecollector.Profile
	Options   onecollector.Options
	Transport http.RoundTripper
}

// UploadOneCollector sends already validated explicit records in-process.
func UploadOneCollector(ctx context.Context, cfg OneCollectorOptions, envelopes []onecollector.Envelope, scrubber journal.Scrubber) (int, error) {
	if scrubber == nil {
		scrubber = journal.NewPatternScrubber()
	}
	for i := range envelopes {
		envelopes[i].IKey = cfg.Options.InstrumentationKey
	}
	pipeline := &oneCollectorPipeline{cfg: cfg, scrubber: scrubber}
	err := pipeline.send(ctx, envelopes)
	return pipeline.lastStatus, err
}

const oneCollectorQueueCount = 1024
const oneCollectorQueueBytes = 8 << 20

type collectorItem struct {
	batch  *collectorBatch
	signal string
	raw    []byte
	event  journal.CommittedEvent
	facts  onecollector.Facts
	flush  chan error
	charge int
}

type collectorBatch struct {
	ctx    context.Context
	failed atomic.Bool
}

type oneCollectorPipeline struct {
	lastStatus int
	cfg        OneCollectorOptions
	scrubber   journal.Scrubber
	mu         sync.Mutex
	queue      []collectorItem
	bytes      int
	count      int
	stopping   bool
	wake       chan struct{}
	done       chan struct{}
	cancel     context.CancelFunc
	dropped    atomic.Uint64
	failures   atomic.Uint64
}

func newOneCollectorPipeline(cfg OneCollectorOptions, scrubber journal.Scrubber) *oneCollectorPipeline {
	ctx, cancel := context.WithCancel(context.Background())
	p := &oneCollectorPipeline{cfg: cfg, scrubber: scrubber, wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	go p.run(ctx)
	return p
}

func (p *oneCollectorPipeline) offer(item collectorItem) {
	encodedFacts, err := json.Marshal(item.facts)
	if err != nil {
		p.dropped.Add(1)
		return
	}
	size := len(item.raw) + len(encodedFacts) + journalLogSize(item.event) + 128
	if !p.mu.TryLock() {
		p.dropped.Add(1)
		return
	}
	defer p.mu.Unlock()
	if p.stopping || p.count >= oneCollectorQueueCount || p.bytes+size > oneCollectorQueueBytes {
		p.dropped.Add(1)
		return
	}
	item.raw = append([]byte(nil), item.raw...)
	item.event.Body = nil
	item.charge = size
	p.queue = append(p.queue, item)
	p.bytes += size
	p.count++
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *oneCollectorPipeline) run(ctx context.Context) {
	defer close(p.done)
	var pending error
	var lastDropped uint64
	for {
		if dropped := p.dropped.Load(); dropped > lastDropped {
			otel.Handle(errors.New("onecollector: records dropped; local journals remain authoritative"))
			lastDropped = dropped
		}
		p.mu.Lock()
		if len(p.queue) == 0 {
			stopping := p.stopping
			p.mu.Unlock()
			if stopping {
				return
			}
			select {
			case <-p.wake:
			case <-ctx.Done():
			}
			continue
		}
		item := p.queue[0]
		p.queue[0] = collectorItem{}
		p.queue = p.queue[1:]
		p.mu.Unlock()
		if item.flush != nil {
			item.flush <- pending
			pending = nil
		} else if ctx.Err() != nil || item.batch != nil && (item.batch.failed.Load() || item.batch.ctx.Err() != nil) {
			p.dropped.Add(1)
		} else {
			exportCtx := ctx
			if item.batch != nil {
				exportCtx = item.batch.ctx
			}
			if err := p.export(exportCtx, item); err != nil {
				if item.batch != nil && (errors.Is(err, errCollectorHTTP) || exportCtx.Err() != nil) {
					item.batch.failed.Store(true)
				}
				p.failures.Add(1)
				pending = err // Retain one bounded diagnostic, not one error per event.
				otel.Handle(err)
			}
		}
		p.mu.Lock()
		p.bytes -= item.charge
		if item.flush == nil {
			p.count--
		}
		p.mu.Unlock()
	}
}

func (p *oneCollectorPipeline) flush(ctx context.Context) error {
	result := make(chan error, 1)
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		select {
		case <-p.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(p.queue) >= oneCollectorQueueCount {
		p.mu.Unlock()
		return errors.New("onecollector: queue full during flush")
	}
	p.queue = append(p.queue, collectorItem{flush: result})
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return onecollector.AwaitLifecycle(ctx, result)
}

func (p *oneCollectorPipeline) shutdown(ctx context.Context) error {
	err := p.flush(ctx)
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		p.cancel()
		<-p.done
		err = errors.Join(err, ctx.Err())
	}
	p.cancel()
	return err
}

func (p *oneCollectorPipeline) export(ctx context.Context, item collectorItem) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	facts := item.facts
	if facts == nil {
		facts = onecollector.Facts{}
	}
	timestamp := item.event.Time.UTC().Format(time.RFC3339Nano)
	switch item.signal {
	case "journal":
		raw := item.raw // The committed bytes already passed the journal boundary scrubber.
		object, err := onecollector.DecodeObject(string(raw), onecollector.MaxNativeBytes)
		if err != nil {
			return err
		}
		for key, value := range object {
			facts["journal."+key] = onecollector.JSON(value)
		}
		facts["journal.raw_json"] = onecollector.JSON(string(raw))
		facts["journal.event_type"] = onecollector.JSON(object["type"])
		facts["journal.run_id"] = onecollector.JSON(item.event.RunID)
		facts["journal.journal_id"] = onecollector.JSON(item.event.JournalID)
		facts["journal.seq"] = onecollector.JSON(strconv.FormatUint(item.event.Seq, 10))
		facts["journal.kind"] = onecollector.JSON(item.event.Kind)
		facts["journal.timestamp"] = onecollector.JSON(timestamp)
	case "trace":
		facts["trace.raw_json"] = onecollector.JSON(string(item.raw))
	}
	if raw, exists := facts["timestamp"]; exists {
		_ = json.Unmarshal(raw, &timestamp)
	}
	var errs []error
	for _, mapping := range p.cfg.Profile.Mappings {
		if mapping.Signal != item.signal || !mapping.Matches(facts) {
			continue
		}
		if item.signal == "metric" && string(facts["metric.synthetic"]) == "true" {
			if err := p.exportHistogram(ctx, mapping, facts, timestamp); err != nil {
				errs = append(errs, err)
				if ctx.Err() != nil || errors.Is(err, errCollectorHTTP) {
					return errors.Join(errs...)
				}
			}
			continue
		}
		if item.signal == "metric" && collectorNeedsInteger(mapping) && facts["metric.integer_value"] == nil {
			errs = append(errs, errors.New("onecollector: metric integer range"))
			continue
		}
		envelope, err := mapping.Envelope(facts, p.cfg.Options.InstrumentationKey, timestamp)
		if err == nil {
			err = p.send(ctx, []onecollector.Envelope{envelope})
		}
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

var errCollectorHTTP = errors.New("onecollector: delivery failed")

func (p *oneCollectorPipeline) send(ctx context.Context, envelopes []onecollector.Envelope) error {
	if _, err := onecollector.Serialize(envelopes); err != nil {
		if len(envelopes) <= 1 {
			return err
		}
		middle := len(envelopes) / 2
		if err := p.send(ctx, envelopes[:middle]); err != nil {
			return err
		}
		return p.send(ctx, envelopes[middle:])
	}
	// Scrub output strings, not serialized JSON syntax, so a registered value
	// cannot turn a valid numeric token or field delimiter into invalid JSON.
	for i := range envelopes {
		if envelopes[i].PreserveInput {
			// Explicit inputs are validated caller payloads, not daemon telemetry.
			envelopes[i].IKey = p.cfg.Options.InstrumentationKey
			continue
		}
		raw, err := json.Marshal(envelopes[i])
		if err != nil {
			return errors.New("onecollector: invalid projected record")
		}
		object, err := onecollector.DecodeObject(string(raw), onecollector.MaxBatchBytes)
		if err != nil {
			return err
		}
		data, _ := object["data"].(map[string]any)
		ext, extOK := object["ext"].(map[string]any)
		if !extOK {
			return errors.New("onecollector: invalid projected objects")
		}
		preserve := func(source map[string]any, names map[string]bool) map[string]any {
			values := make(map[string]any)
			for name := range names {
				if value, found := source[name]; found {
					values[name] = value
					delete(source, name)
				}
			}
			return values
		}
		rawData := preserve(data, envelopes[i].RawDataFields)
		rawExt := preserve(ext, envelopes[i].RawExtensionFields)
		if err := scrubCollectorValue(object, p.scrubber); err != nil {
			return errors.New("onecollector: invalid scrubbed record")
		}
		for name, value := range rawData {
			data[name] = value
		}
		for name, value := range rawExt {
			ext[name] = value
		}
		envelopes[i].Data = object["data"]
		extensions, ok := object["ext"].(map[string]any)
		if !ok {
			return errors.New("onecollector: invalid extensions")
		}
		envelopes[i].Ext = extensions
		envelopes[i].Name, _ = object["name"].(string)
		envelopes[i].Ver, _ = object["ver"].(string)
		// Routing is transport metadata, not payload facts.
		envelopes[i].IKey = p.cfg.Options.InstrumentationKey
	}
	status, err := onecollector.SendResult(ctx, p.cfg.Options, envelopes, p.cfg.Transport)
	p.lastStatus = status
	if err != nil {
		return errors.Join(errCollectorHTTP, err)
	}
	return nil
}

func scrubCollectorValue(value any, scrubber journal.Scrubber) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			cleanKey := string(scrubber.Scrub([]byte(key)))
			if cleanKey != key {
				if _, exists := typed[cleanKey]; exists {
					return errors.New("onecollector: scrubbed key collision")
				}
				delete(typed, key)
			}
			if text, ok := child.(string); ok {
				typed[cleanKey] = string(scrubber.Scrub([]byte(text)))
			} else {
				if err := scrubCollectorValue(child, scrubber); err != nil {
					return err
				}
				typed[cleanKey] = child
			}
		}
	case []any:
		for i, child := range typed {
			if text, ok := child.(string); ok {
				typed[i] = string(scrubber.Scrub([]byte(text)))
			} else {
				if err := scrubCollectorValue(child, scrubber); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type collectorSpanProcessor struct{ pipeline *oneCollectorPipeline }

func (s collectorSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (s collectorSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	p := s.pipeline
	raw, err := (&JournalSpanExporter{scrubber: p.scrubber}).marshalOTLP([]sdktrace.ReadOnlySpan{span})
	if err != nil || len(raw) > onecollector.MaxNativeBytes {
		p.dropped.Add(1)
		otel.Handle(errors.New("onecollector: trace conversion or byte limit"))
		return
	}
	facts := onecollector.Facts{
		"trace.name":       onecollector.JSON(redactWith(p.scrubber, span.Name())),
		"trace.trace_id":   onecollector.JSON(span.SpanContext().TraceID().String()),
		"trace.span_id":    onecollector.JSON(span.SpanContext().SpanID().String()),
		"trace.start_time": onecollector.JSON(strconv.FormatInt(span.StartTime().UnixNano(), 10)),
		"trace.end_time":   onecollector.JSON(strconv.FormatInt(span.EndTime().UnixNano(), 10)),
		"timestamp":        onecollector.JSON(span.StartTime().UTC().Format(time.RFC3339Nano)),
	}
	addCollectorAttrs(facts, "attributes", span.Attributes(), p.scrubber)
	addCollectorAttrs(facts, "resource", span.Resource().Attributes(), p.scrubber)
	p.offer(collectorItem{signal: "trace", raw: raw, facts: facts})
}
func (s collectorSpanProcessor) Shutdown(context.Context) error   { return nil }
func (s collectorSpanProcessor) ForceFlush(context.Context) error { return nil }

func addCollectorAttrs(facts onecollector.Facts, prefix string, attrs []attribute.KeyValue, scrubber journal.Scrubber) {
	for _, attr := range attrs {
		selector := prefix + "." + string(attr.Key)
		switch attr.Value.Type() {
		case attribute.BOOL, attribute.INT64, attribute.FLOAT64, attribute.STRING:
			if attr.Value.Type() == attribute.FLOAT64 && (math.IsNaN(attr.Value.AsFloat64()) || math.IsInf(attr.Value.AsFloat64(), 0)) {
				continue
			}
			facts[selector] = onecollector.JSON(attr.Value.AsInterface())
		default:
			raw, err := protojson.Marshal((&JournalSpanExporter{scrubber: scrubber}).otlpAttributeValue(attr.Value))
			if err != nil {
				continue
			}
			facts[selector] = onecollector.JSON(string(raw))
			facts[selector+".otlp_type"] = onecollector.JSON("arrayValue")
		}
	}
}

type collectorMetricExporter struct {
	pipeline *oneCollectorPipeline
	batch    *collectorBatch
}

func (c *Client) attachOneCollector(cfg Config) error {
	if c.oneCollector == nil || !c.oneCollector.cfg.Profile.HasSignal("journal") || cfg.JournalRoot == "" || c.unregisterJournal != nil {
		return nil
	}
	unregister, err := journal.RegisterCommittedEventSink(cfg.JournalRoot, cfg.JournalInstanceID, c)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.oneCollector.shutdown(ctx)
		c.oneCollector = nil
		return fmt.Errorf("%w: onecollector: journal registration failed", ErrOTLPUnavailable)
	}
	c.unregisterJournal = unregister
	return nil
}

func (c *Client) startOneCollectorFlush(ctx context.Context) func() {
	if c.oneCollector == nil {
		return func() {}
	}
	done := make(chan error, 1)
	go func() { done <- c.oneCollector.flush(ctx) }()
	return func() {
		if err := onecollector.AwaitLifecycle(ctx, done); err != nil {
			otel.Handle(err)
		}
		// Metrics may be collected after the first barrier was queued.
		if err := c.oneCollector.flush(ctx); err != nil {
			otel.Handle(err)
		}
	}
}

func (e collectorMetricExporter) Temporality(metric.InstrumentKind) metricdata.Temporality {
	return metricdata.DeltaTemporality
}
func (e collectorMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return metric.DefaultAggregationSelector(kind)
}
func (e collectorMetricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	e.batch = &collectorBatch{ctx: ctx}
	var errs []error
	for _, scope := range data.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if err := e.exportInstrument(ctx, data, scope, instrument); err != nil {
				errs = append(errs, err)
				if ctx.Err() != nil {
					break
				}
			}
		}
	}
	return errors.Join(errors.Join(errs...), e.pipeline.flush(ctx))
}
func (e collectorMetricExporter) ForceFlush(context.Context) error { return nil }
func (e collectorMetricExporter) Shutdown(context.Context) error   { return nil }

func metricPointDigest(facts onecollector.Facts) string {
	raw, _ := json.Marshal(facts)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func collectorNeedsInteger(mapping onecollector.Mapping) bool {
	if mapping.DataSource == "metric.integer_value" {
		return true
	}
	for _, fields := range []map[string]onecollector.Field{mapping.Fields, mapping.Extensions} {
		for _, field := range fields {
			if field.Source == "metric.integer_value" {
				return true
			}
		}
	}
	return false
}

func (p *oneCollectorPipeline) exportHistogram(ctx context.Context, mapping onecollector.Mapping, facts onecollector.Facts, timestamp string) error {
	var count uint64
	var counts []uint64
	var bounds []float64
	var sum float64
	for raw, target := range map[string]any{"metric.count": &count, "histogram.counts": &counts, "histogram.bounds": &bounds, "metric.sum": &sum} {
		if json.Unmarshal(facts[raw], target) != nil {
			return errors.New("onecollector: histogram decode")
		}
	}
	var min, max *float64
	if raw, ok := facts["metric.min"]; ok {
		var value float64
		if json.Unmarshal(raw, &value) != nil {
			return errors.New("onecollector: histogram minimum")
		}
		min = &value
	}
	if raw, ok := facts["metric.max"]; ok {
		var value float64
		if json.Unmarshal(raw, &value) != nil {
			return errors.New("onecollector: histogram maximum")
		}
		max = &value
	}
	segments, err := onecollector.Replay(count, counts, bounds, &sum, min, max, mapping.Nonnegative)
	if err != nil {
		return err
	}
	if collectorNeedsInteger(mapping) {
		for _, segment := range segments {
			if _, ok := collectorInteger(segment.Value); !ok {
				return errors.New("onecollector: histogram integer range")
			}
		}
	}
	batch := make([]onecollector.Envelope, 0, onecollector.MaxRecords)
	batchBytes := 0
	var index uint64
	for _, segment := range segments {
		for n := uint64(0); n < segment.Count; n++ {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			facts["metric.value"] = onecollector.JSON(segment.Value)
			if value, ok := collectorInteger(segment.Value); ok {
				facts["metric.integer_value"] = onecollector.JSON(value)
			}
			facts["metric.sample_index"] = onecollector.JSON(strconv.FormatUint(index, 10))
			index++
			envelope, err := mapping.Envelope(facts, p.cfg.Options.InstrumentationKey, timestamp)
			if err != nil {
				return err
			}
			encoded, err := onecollector.Serialize([]onecollector.Envelope{envelope})
			if err != nil {
				return err
			}
			if len(batch) > 0 && batchBytes+len(encoded) > onecollector.MaxBatchBytes {
				if err := p.send(ctx, batch); err != nil {
					return err
				}
				clear(batch)
				batch = batch[:0]
				batchBytes = 0
			}
			batch = append(batch, envelope)
			batchBytes += len(encoded)
			if len(batch) == onecollector.MaxRecords {
				if err := p.send(ctx, batch); err != nil {
					return err
				}
				clear(batch)
				batch = batch[:0]
				batchBytes = 0
			}
		}
	}
	if len(batch) > 0 {
		return p.send(ctx, batch)
	}
	return nil
}

func (e collectorMetricExporter) exportInstrument(ctx context.Context, data *metricdata.ResourceMetrics, scope metricdata.ScopeMetrics, instrument metricdata.Metrics) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	base := onecollector.Facts{
		"metric.name": onecollector.JSON(instrument.Name), "metric.unit": onecollector.JSON(instrument.Unit),
		"metric.scope_name": onecollector.JSON(scope.Scope.Name), "metric.scope_version": onecollector.JSON(scope.Scope.Version),
		"metric.scope_schema_url":    onecollector.JSON(scope.Scope.SchemaURL),
		"metric.resource_schema_url": onecollector.JSON(data.Resource.SchemaURL()),
	}
	addCollectorAttrs(base, "resource", data.Resource.Attributes(), e.pipeline.scrubber)
	addCollectorAttrs(base, "scope", scope.Scope.Attributes.ToSlice(), e.pipeline.scrubber)
	switch aggregation := instrument.Data.(type) {
	case metricdata.Sum[int64]:
		return offerCollectorPoints(e.pipeline, e.batch, base, aggregation.DataPoints, "sum", aggregation.Temporality, &aggregation.IsMonotonic)
	case metricdata.Sum[float64]:
		return offerCollectorPoints(e.pipeline, e.batch, base, aggregation.DataPoints, "sum", aggregation.Temporality, &aggregation.IsMonotonic)
	case metricdata.Gauge[int64]:
		return offerCollectorPoints(e.pipeline, e.batch, base, aggregation.DataPoints, "gauge", 0, nil)
	case metricdata.Gauge[float64]:
		return offerCollectorPoints(e.pipeline, e.batch, base, aggregation.DataPoints, "gauge", 0, nil)
	case metricdata.Histogram[float64]:
		return offerCollectorHistograms(e.pipeline, e.batch, base, aggregation)
	case metricdata.Histogram[int64]:
		converted := metricdata.Histogram[float64]{Temporality: aggregation.Temporality}
		var errs []error
		for _, point := range aggregation.DataPoints {
			min, hasMin := point.Min.Value()
			max, hasMax := point.Max.Value()
			exact := func(value int64) bool {
				floating := float64(value)
				return floating < 0x1p63 && int64(floating) == value
			}
			if !exact(point.Sum) || hasMin && !exact(min) || hasMax && !exact(max) {
				errs = append(errs, errors.New("onecollector: histogram integer precision"))
				continue
			}
			p := metricdata.HistogramDataPoint[float64]{Attributes: point.Attributes, StartTime: point.StartTime, Time: point.Time,
				Count: point.Count, BucketCounts: point.BucketCounts, Bounds: point.Bounds, Sum: float64(point.Sum)}
			if hasMin {
				p.Min = metricdata.NewExtrema(float64(min))
			}
			if hasMax {
				p.Max = metricdata.NewExtrema(float64(max))
			}
			converted.DataPoints = append(converted.DataPoints, p)
		}
		return errors.Join(errors.Join(errs...), offerCollectorHistograms(e.pipeline, e.batch, base, converted))
	default:
		return fmt.Errorf("onecollector: unsupported metric aggregation")
	}
}
