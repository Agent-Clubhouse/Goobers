package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/goobers/goobers/internal/journal"
)

type journalBatchTestExporter struct {
	journalTestExporter
	sizes     []int
	bodyBytes []int
}

func (e *journalBatchTestExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := e.journalTestExporter.Export(ctx, records); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sizes = append(e.sizes, len(records))
	size := 0
	for i := range records {
		size += len(records[i].Body().AsString())
	}
	e.bodyBytes = append(e.bodyBytes, size)
	return nil
}

func TestJournalLogsBatchesRespectCountBytesAndFlushTail(t *testing.T) {
	for _, bodySize := range []int{1024, 32 << 10, 300 << 10} {
		t.Run(strconv.Itoa(bodySize), func(t *testing.T) {
			exporter := &journalBatchTestExporter{journalTestExporter: journalTestExporter{
				started: make(chan struct{}), release: make(chan struct{}),
			}}
			client := journalTestClient(t, exporter)
			client.Commit(journal.CommittedEvent{JournalID: "batch", Body: []byte("{}")})
			waitJournalStarted(t, exporter.started)
			count := 300
			if bodySize > 1024 {
				count = 17
			}
			for i := range count {
				client.Commit(journal.CommittedEvent{JournalID: "batch", Seq: uint64(i + 1), Body: []byte(strings.Repeat("x", bodySize))})
			}
			close(exporter.release)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.journalLogs.flush(ctx); err != nil {
				t.Fatal(err)
			}
			exporter.mu.Lock()
			defer exporter.mu.Unlock()
			if len(exporter.records) != count+1 {
				t.Fatalf("records=%d want=%d", len(exporter.records), count+1)
			}
			for i, size := range exporter.sizes {
				if size > journalLogBatchLimit || (size > 1 && exporter.bodyBytes[i] > journalLogBatchBytes) {
					t.Fatalf("unbounded batch: records=%d bodyBytes=%d", size, exporter.bodyBytes[i])
				}
			}
			if bodySize <= 32<<10 && len(exporter.sizes) >= count {
				t.Fatalf("no batching: %v", exporter.sizes)
			}
		})
	}
}

func TestJournalLogsReplayInspectionDoesNotHoldProducerLock(t *testing.T) {
	exporter := &journalTestExporter{started: make(chan struct{}), release: make(chan struct{})}
	client := journalTestClient(t, exporter)
	t.Cleanup(func() { close(exporter.release) })
	client.Commit(journal.CommittedEvent{JournalID: "stats", Body: []byte("{}")})
	waitJournalStarted(t, exporter.started)
	started, release := make(chan struct{}), make(chan struct{})
	client.journalLogs.replayStats = func() AzureReplayStats { close(started); <-release; return AzureReplayStats{} }
	done := make(chan struct{})
	go func() { _ = client.JournalExportStats(); close(done) }()
	defer func() { close(release); <-done }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stats did not reach replay inspection")
	}
	client.Commit(journal.CommittedEvent{JournalID: "stats", Body: []byte("{}")})
	if got := client.journalLogs.accepted.Load(); got != 2 {
		t.Fatalf("replay inspection blocked admission: accepted=%d", got)
	}
}

// This fixture exercises the production journal worker, Azure serialization,
// gzip, fsynced replay files, and HTTP delivery. It does NOT simulate workflow
// execution or the authoritative journal's own fsync.
type journalLoadReceiver struct {
	mu                                    sync.Mutex
	requests, records, wireBytes, largest int
	malformed                             bool
}

func newJournalLoadClient(tb testing.TB, receiver *journalLoadReceiver, configure ...func(*Config)) *Client {
	tb.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		count := 0
		valid := true
		for _, line := range bytesLines(body) {
			if len(line) == 0 {
				continue
			}
			count++
			valid = valid && json.Valid(line)
		}
		receiver.mu.Lock()
		receiver.requests++
		receiver.records += count
		receiver.wireBytes += len(body)
		receiver.largest = max(receiver.largest, count)
		receiver.malformed = receiver.malformed || !valid
		receiver.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	tb.Cleanup(server.Close)
	cfg := Config{
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorHTTPClient:       server.Client(), AzureMonitorJournalLogs: true, JournalLogs: true, JournalLogsOnly: true,
		AzureMonitorReplayRoot: tb.TempDir(), AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 32 << 20,
	}
	for _, option := range configure {
		option(&cfg)
	}
	client, err := New(context.Background(), cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Shutdown(ctx)
	})
	return client
}

func settleJournalLoad(tb testing.TB, client *Client) JournalExportStats {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.journalLogs.flush(ctx); err != nil {
		tb.Fatal(err)
	}
	for {
		stats := client.JournalExportStats()
		if stats.AzureReplay.Delivered == stats.Accepted && stats.AzureReplay.PendingRecords == 0 {
			return stats
		}
		if ctx.Err() != nil {
			tb.Fatalf("load did not settle: %+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJournalLogsLoadUsesBatchedDurableHTTP(t *testing.T) {
	var receiver journalLoadReceiver
	client := newJournalLoadClient(t, &receiver)
	const count = 2048
	body := []byte(`{"event":"load","padding":"` + strings.Repeat("x", 1024) + `"}`)
	for i := range count {
		client.Commit(journal.CommittedEvent{JournalID: "load", Seq: uint64(i), Body: body})
		if (i+1)%256 == 0 {
			settleJournalLoad(t, client)
		}
	}
	stats := settleJournalLoad(t, client)
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if stats.Accepted+stats.Dropped != count || stats.ExportFailures != 0 || stats.AzureReplay.Retried != 0 ||
		uint64(receiver.records) != stats.Accepted || receiver.malformed || receiver.largest > journalLogBatchLimit {
		t.Fatalf("load accounting/bounds: stats=%+v receiver=%+v", stats, &receiver)
	}
	if receiver.records != count || stats.Dropped != 0 || receiver.requests > count/32 {
		t.Fatalf("insufficient throughput/batching: accepted=%d requests=%d drops=%d", receiver.records, receiver.requests, stats.Dropped)
	}
	t.Logf("records=%d HTTP requests=%d records/request=%.1f drops=%d", receiver.records, receiver.requests, float64(receiver.records)/float64(receiver.requests), stats.Dropped)
}

func BenchmarkJournalLogsDurableHTTP(b *testing.B) {
	var receiver journalLoadReceiver
	client := newJournalLoadClient(b, &receiver)
	body := []byte(`{"event":"load","padding":"` + strings.Repeat("x", 1024) + `"}`)
	samples := make([]time.Duration, 0, min(b.N, 100000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		start := time.Now()
		client.Commit(journal.CommittedEvent{JournalID: "load", Seq: uint64(i), Body: body})
		if len(samples) < cap(samples) {
			samples = append(samples, time.Since(start))
		}
		if (i+1)%256 == 0 {
			settleJournalLoad(b, client)
		}
	}
	stats := settleJournalLoad(b, client)
	b.StopTimer()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Nanoseconds()), "commit-p95-ns")
		b.ReportMetric(float64(samples[(len(samples)-1)*99/100].Nanoseconds()), "commit-p99-ns")
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	b.ReportMetric(float64(receiver.records)/float64(max(receiver.requests, 1)), "records/request")
	b.ReportMetric(float64(stats.Dropped), "drops")
	b.ReportMetric(float64(receiver.wireBytes)/float64(max(receiver.records, 1)), "encoded-B/record")
}

// Measure the production catch-up notification path with an empty and a full
// bounded hint queue. There is deliberately no consumer: deferred wake hints
// are accounted here; recovery from the journal is tested separately.
func BenchmarkJournalCatchupCommitHint(b *testing.B) {
	for _, full := range []bool{false, true} {
		b.Run(fmt.Sprintf("queue-full=%t", full), func(b *testing.B) {
			pipeline := &journalLogPipeline{}
			source := &journalCatchup{pipeline: pipeline, hints: make(chan journalCatchupHint, 1024)}
			client := &Client{journalLogs: pipeline, journalCatchup: source}
			if full {
				for range cap(source.hints) {
					source.offer(journalCatchupHint{})
				}
			}
			event := journal.CommittedEvent{Kind: "run", RunID: "0123456789abcdef0123456789abcdef", JournalID: "0123456789abcdef0123456789abcdef", Body: make([]byte, 32<<10)}
			samples := make([]time.Duration, 0, min(b.N, 100000))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				started := time.Now()
				client.Commit(event)
				if len(samples) < cap(samples) {
					samples = append(samples, time.Since(started))
				}
				if !full {
					<-source.hints
				}
			}
			b.StopTimer()
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			b.ReportMetric(float64(samples[(len(samples)-1)*99/100].Nanoseconds()), "commit-p99-ns")
			want := uint64(0)
			if full {
				want = uint64(b.N)
			}
			if pipeline.catchupDeferred.Load() != want {
				b.Fatal("deferred hint accounting mismatch")
			}
		})
	}
}
