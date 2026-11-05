package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

func TestOneCollectorMetricTemporality(t *testing.T) {
	for _, tt := range []struct {
		name string
		kind metric.InstrumentKind
		want metricdata.Temporality
	}{
		{"counter", metric.InstrumentKindCounter, metricdata.DeltaTemporality},
		{"observable counter", metric.InstrumentKindObservableCounter, metricdata.DeltaTemporality},
		{"histogram", metric.InstrumentKindHistogram, metricdata.DeltaTemporality},
		{"up down counter", metric.InstrumentKindUpDownCounter, metricdata.CumulativeTemporality},
		{"observable up down counter", metric.InstrumentKindObservableUpDownCounter, metricdata.CumulativeTemporality},
		{"gauge", metric.InstrumentKindGauge, metric.DefaultTemporalitySelector(metric.InstrumentKindGauge)},
		{"observable gauge", metric.InstrumentKindObservableGauge, metric.DefaultTemporalitySelector(metric.InstrumentKindObservableGauge)},
		{"unknown", metric.InstrumentKind(255), metric.DefaultTemporalitySelector(metric.InstrumentKind(255))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := (collectorMetricExporter{}).Temporality(tt.kind); got != tt.want {
				t.Fatalf("temporality = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOneCollectorUpDownCountersCumulativeProjection(t *testing.T) {
	const observableName = "synthetic.observable.active"
	mapping := syntheticMapping("metric")
	mapping.Match = map[string][]string{
		"metric.name":        {MetricWorkActive, observableName},
		"metric.unit":        {"{span}"},
		"metric.kind":        {"sum"},
		"metric.temporality": {"cumulative"},
	}
	mapping.Fields["name"] = onecollector.Field{Source: "metric.name"}
	mapping.Fields["temporality"] = onecollector.Field{Source: "metric.temporality"}
	mapping.Fields["monotonic"] = onecollector.Field{Source: "metric.monotonic"}
	var mu sync.Mutex
	values := make(map[string][]int64)
	cfg := collectorFixture(t, []onecollector.Mapping{mapping}, func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		for decoder.More() {
			var envelope struct {
				Data struct {
					Name        string `json:"name"`
					Value       int64  `json:"value"`
					Temporality string `json:"temporality"`
					Monotonic   bool   `json:"monotonic"`
				} `json:"data"`
			}
			if err := decoder.Decode(&envelope); err != nil {
				t.Error(err)
				break
			}
			if envelope.Data.Temporality != "cumulative" || envelope.Data.Monotonic {
				t.Errorf("invalid up/down counter projection: %+v", envelope.Data)
			}
			mu.Lock()
			values[envelope.Data.Name] = append(values[envelope.Data.Name], envelope.Data.Value)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
	client, err := New(t.Context(), Config{OneCollector: &cfg, NoDefaultExporter: true, MetricExportInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	_, err = client.meterProvider.Meter("synthetic").Int64ObservableUpDownCounter(observableName,
		apimetric.WithUnit("{span}"),
		apimetric.WithInt64Callback(func(_ context.Context, observer apimetric.Int64Observer) error {
			observer.Observe(3)
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	_, span, err := client.StartRun(t.Context(), RunAttributes{Gaggle: "synthetic", WorkflowID: "synthetic", RunID: testRunID})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := client.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	span.Complete(OutcomeSuccess, false)
	if err := client.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(values[MetricWorkActive], []int64{1, 1, 0}) {
		t.Fatalf("active work = %v, want [1 1 0]", values[MetricWorkActive])
	}
	if !slices.Equal(values[observableName], []int64{3, 3, 3}) {
		t.Fatalf("observable active = %v, want [3 3 3]", values[observableName])
	}
}

func TestOneCollectorExplicitDeliveryStatusScrubbingAndNoRetry(t *testing.T) {
	for _, status := range []int{202, 302, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("journal")}, func(w http.ResponseWriter, r *http.Request) {
				calls++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if strings.Contains(string(raw), "synthetic-payload-secret") || !strings.Contains(string(raw), "9223372036854775807") {
					t.Error("payload scrubbing or integer precision failed")
				}
				w.Header().Set("Location", "https://invalid.example.test/private")
				w.WriteHeader(status)
			})
			registry := journal.NewRegistryScrubber()
			registry.Register([]byte("synthetic-payload-secret"))
			records := []onecollector.Envelope{{Name: "Synthetic.Input", Ver: "4.0", Time: time.Now().UTC().Format(time.RFC3339Nano),
				Ext: map[string]any{}, Data: map[string]any{"text": "synthetic-payload-secret", "value": json.Number("9223372036854775807")}}}
			actual, err := UploadOneCollector(t.Context(), cfg, records, registry)
			if actual != status || calls != 1 {
				t.Fatal(actual, calls)
			}
			if status == 202 && err != nil {
				t.Fatal(err)
			}
			if status != 202 {
				var delivery *onecollector.DeliveryError
				if !errors.As(err, &delivery) || delivery.Category != "http" || delivery.Status != status {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOneCollectorIntHistogramPrecisionAndLaterValidPoint(t *testing.T) {
	calls := 0
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("metric")}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
	})
	p := newOneCollectorPipeline(cfg, journal.NewPatternScrubber())
	exporter := collectorMetricExporter{pipeline: p}
	now := time.Now()
	data := &metricdata.ResourceMetrics{Resource: resource.Empty(), ScopeMetrics: []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: "synthetic"}, Metrics: []metricdata.Metrics{{
		Name: "synthetic.histogram", Unit: "1", Data: metricdata.Histogram[int64]{DataPoints: []metricdata.HistogramDataPoint[int64]{
			{Time: now, Count: 1, Sum: math.MaxInt64, BucketCounts: []uint64{1}},
			{Time: now, Count: 1, Sum: 2, Min: metricdata.NewExtrema(int64(2)), Max: metricdata.NewExtrema(int64(2)), BucketCounts: []uint64{1}},
		}},
	}}}}}
	if err := exporter.Export(context.Background(), data); err == nil {
		t.Fatal("inexact integer histogram accepted")
	}
	if err := p.shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("valid later point was not delivered", calls)
	}
}

func TestOneCollectorExplicitInputPreservesCallerContent(t *testing.T) {
	raw := `{ "secret":"synthetic-payload-secret", "value":9223372036854775807 }`
	calls := 0
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("journal")}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var record struct {
			Data string `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			t.Error(err)
		}
		if record.Data != raw {
			t.Errorf("explicit content changed: %q", record.Data)
		}
		w.WriteHeader(202)
	})
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("synthetic-payload-secret"))
	registry.Register([]byte("9223372036854775807"))
	mapping := onecollector.Mapping{Signal: "input", EventName: "Synthetic.Input", EnvelopeVersion: "4.0", DataSource: "input.body"}
	envelope, err := mapping.Envelope(onecollector.Facts{"input.body": onecollector.JSON(raw)}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UploadOneCollector(t.Context(), cfg, []onecollector.Envelope{envelope}, registry); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestOneCollectorActualCommittedJournalMatchesPersistedText(t *testing.T) {
	var received string
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("journal")}, func(w http.ResponseWriter, r *http.Request) {
		var record struct {
			Data struct {
				Body string `json:"body"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			t.Error(err)
		}
		received = record.Data.Body
		w.WriteHeader(200)
	})
	root := t.TempDir()
	registry := journal.NewRegistryScrubber()
	client, err := New(t.Context(), Config{OneCollector: &cfg, JournalLogsOnly: true, JournalRoot: root, Scrubber: registry})
	if err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"), journal.WithScrubber(registry))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventDaemonStarted, Reason: "synthetic-persisted"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	registry.Register([]byte("synthetic-persisted"))
	if err := client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "scheduler", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	persisted, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if received != strings.TrimSuffix(string(persisted), "\n") {
		t.Fatal("committed body differs from persisted journal")
	}
}
