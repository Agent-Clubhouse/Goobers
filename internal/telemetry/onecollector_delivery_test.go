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
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

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
