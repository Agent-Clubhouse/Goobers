package telemetry

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Explicit release experiment, not an ordinary CI test. With ten workflows,
// 38 records/run and a three-minute period, nominal journal rate is 2.11/s.
// This offers 212/s for 60 seconds across ten independent durable journals.
// Use -benchtime=1x; -count=3 repeats the baseline/enabled comparisons.
// It measures journal append + catch-up/replay, not workflow stage dispatch.
func BenchmarkJournalCatchupRateControlledBurst(b *testing.B) {
	benchmarkJournalRateCases(b, 212*60)
}

// Representative-rate component experiment: 127 records/minute approximates
// ten workflows producing 38 records each every three minutes. This measures
// durable journal Append directly, separately from workflow/polling latency.
// It does not include the daemon's other signals or prove stage-dispatch tails.
// Use -benchtime=1x and repeat matched baseline/enabled cases on the target host.
func BenchmarkJournalCatchupNormalRate(b *testing.B) {
	benchmarkJournalRateCases(b, 127)
}

func benchmarkJournalRateCases(b *testing.B, count int) {
	b.Helper()
	for _, size := range []int{1024, 32 << 10} {
		for _, enabled := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/enabled=%t", size, enabled), func(b *testing.B) {
				if b.N != 1 {
					b.Fatal("use -benchtime=1x for the fixed 60-second experiment")
				}
				benchmarkJournalRate(b, size, enabled, count)
			})
		}
	}
}

type burstReceiver struct {
	mu                         sync.Mutex
	keys                       map[string]string
	duplicates                 int
	malformed                  bool
	requests, records, largest int
}

func TestJournalCatchupBurstReceiverIdentityAccounting(t *testing.T) {
	receiver := &burstReceiver{keys: make(map[string]string)}
	for i, id := range []string{"stable", "stable", "changed"} {
		var body bytes.Buffer
		writer := gzip.NewWriter(&body)
		_, err := fmt.Fprintf(writer, `{"data":{"baseData":{"properties":{"goobers.journal.kind":"run","goobers.journal.id":"run","goobers.journal.seq":"1","goobers.telemetry.record_id":%q}}}}`, id)
		if err != nil {
			t.Fatal(err)
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		receiver.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", &body))
		if response.Code != http.StatusOK || len(receiver.keys) != 1 || receiver.duplicates != i || receiver.malformed != (i == 2) {
			t.Fatalf("identity accounting after record %d: %+v", i, receiver)
		}
	}
}

func (s *burstReceiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	reader, err := gzip.NewReader(request.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer func() { _ = reader.Close() }()
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 2<<20)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	batch := 0
	for scan.Scan() {
		batch++
		s.records++
		var envelope struct {
			Data struct {
				BaseData struct{ Properties map[string]string }
			}
		}
		if json.Unmarshal(scan.Bytes(), &envelope) != nil {
			s.malformed = true
			continue
		}
		p := envelope.Data.BaseData.Properties
		key, id := p["goobers.journal.id"]+":"+p["goobers.journal.seq"], p["goobers.telemetry.record_id"]
		if p["goobers.journal.kind"] != "run" || id == "" || key == ":" {
			s.malformed = true
		}
		if prior, exists := s.keys[key]; exists {
			s.duplicates++
			if prior != id {
				s.malformed = true
			}
		}
		s.keys[key] = id
	}
	s.largest = max(s.largest, batch)
	if scan.Err() != nil {
		s.malformed = true
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func benchmarkJournalRate(b *testing.B, size int, enabled bool, count int) {
	b.Helper()
	const window, writers = time.Minute, 10
	root, spool := b.TempDir(), b.TempDir()
	receiver := &burstReceiver{keys: make(map[string]string)}
	server := httptest.NewServer(receiver)
	defer server.Close()
	var client *Client
	if enabled {
		var err error
		client, err = New(b.Context(), Config{
			AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
			AzureMonitorHTTPClient:       server.Client(), AzureMonitorJournalLogs: true, JournalLogs: true, JournalLogsOnly: true,
			JournalRoot: root, JournalInstanceID: "burst-instance", AzureMonitorReplayRoot: spool,
			AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 512 << 20,
		})
		if err != nil {
			b.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Shutdown(ctx); err != nil {
				b.Error(err)
			}
		}()
	}
	runs := make([]*journal.Run, writers)
	paths := make([]string, writers)
	for i := range runs {
		id := fmt.Sprintf("%032x", i+1)
		var err error
		runs[i], err = journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{RunID: id, Workflow: "burst", InstanceID: "burst-instance"}, nil)
		if err != nil {
			b.Fatal(err)
		}
		paths[i] = filepath.Join(root, "runs", id, "events.jsonl")
		defer func() { _ = runs[i].Close() }()
	}
	latencies := make([][]time.Duration, writers)
	errors := make(chan error, writers)
	var wg sync.WaitGroup
	event := journal.Event{Type: journal.EventRunStarted, Reason: strings.Repeat("x", size)}
	b.ResetTimer()
	start := time.Now()
	for worker := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < count; i += writers {
				time.Sleep(time.Until(start.Add(time.Duration(i) * window / time.Duration(count))))
				appendStart := time.Now()
				if err := runs[worker].Append(event); err != nil {
					errors <- err
					return
				}
				latencies[worker] = append(latencies[worker], time.Since(appendStart))
			}
		}()
	}
	wg.Wait()
	producing := time.Since(start)
	b.StopTimer()
	close(errors)
	for err := range errors {
		b.Fatal(err)
	}
	var all []time.Duration
	for i, run := range runs {
		if err := run.Close(); err != nil {
			b.Fatal(err)
		}
		all = append(all, latencies[i]...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	b.ReportMetric(float64(count)/window.Seconds(), "offered-records/s")
	b.ReportMetric(float64(count)/producing.Seconds(), "achieved-records/s")
	b.ReportMetric(float64(all[(len(all)-1)*95/100].Nanoseconds()), "append-p95-ns")
	b.ReportMetric(float64(all[(len(all)-1)*99/100].Nanoseconds()), "append-p99-ns")
	// The testing package omits the benchmark metric row when a subcase fails.
	// Keep source timings in its failure log too; an unsustained offered rate
	// must not erase the evidence needed to compare disabled and enabled runs.
	b.Logf("source: payload=%d enabled=%t records=%d elapsed=%s achieved=%.3f/s append_p95=%s append_p99=%s",
		size, enabled, count, producing, float64(count)/producing.Seconds(),
		all[(len(all)-1)*95/100], all[(len(all)-1)*99/100])
	if producing > 66*time.Second {
		b.Errorf("producer could not sustain offered rate: %d records in %s", count, producing)
	}
	expected := burstJournalKeys(b, paths)
	// Create publishes one initial run.started record per journal before the
	// timed burst. Include those in reconciliation, not the offered-rate count.
	if len(expected) != count+writers {
		b.Fatalf("authoritative journal count=%d want=%d", len(expected), count+writers)
	}
	if enabled {
		settleBurst(b, client, receiver, expected)
	}
}

func burstJournalKeys(b *testing.B, paths []string) []string {
	b.Helper()
	var keys []string
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		scan := bufio.NewScanner(file)
		scan.Buffer(make([]byte, 4096), 8<<20)
		for scan.Scan() {
			var event struct{ Seq uint64 }
			if err = json.Unmarshal(scan.Bytes(), &event); err != nil {
				b.Fatal(err)
			}
			keys = append(keys, filepath.Base(filepath.Dir(path))+":"+strconv.FormatUint(event.Seq, 10))
		}
		if err = scan.Err(); err != nil {
			b.Fatal(err)
		}
		if err = file.Close(); err != nil {
			b.Fatal(err)
		}
	}
	return keys
}

func settleBurst(b *testing.B, client *Client, receiver *burstReceiver, expected []string) {
	b.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(b.Context(), time.Minute)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		b.Fatal(err)
	}
	for {
		receiver.mu.Lock()
		missing := 0
		identities := make(map[string]bool, len(expected))
		for _, key := range expected {
			id := receiver.keys[key]
			if id == "" {
				missing++
			} else {
				identities[id] = true
			}
		}
		malformed, duplicates := receiver.malformed, receiver.duplicates
		requests, records, largest := receiver.requests, receiver.records, receiver.largest
		receiver.mu.Unlock()
		if malformed || largest > azureReplayBatchRecords || (missing == 0 && len(identities) != len(expected)) {
			b.Fatal("malformed record, unstable duplicate ID or identity collision")
		}
		if missing == 0 {
			b.ReportMetric(float64(time.Since(start).Milliseconds()), "recovery-ms")
			b.ReportMetric(float64(duplicates), "duplicate-copies")
			b.ReportMetric(float64(records)/float64(max(requests, 1)), "records/request")
			b.ReportMetric(float64(largest), "max-records/request")
			stats := client.JournalExportStats()
			b.ReportMetric(float64(stats.Dropped), "export-drops")
			b.ReportMetric(float64(stats.CatchupDeferred), "deferred-hints")
			b.Logf("reconciliation: expected=%d unique=%d missing=%d recovery=%s requests=%d records=%d largest_batch=%d duplicates=%d dropped=%d deferred=%d pruned_age=%d pruned_bytes=%d",
				len(expected), len(identities), missing, time.Since(start), requests, records, largest,
				duplicates, stats.Dropped, stats.CatchupDeferred, stats.AzureReplay.PrunedAge, stats.AzureReplay.PrunedBytes)
			if stats.AzureReplay.PrunedBytes != 0 || stats.AzureReplay.PrunedAge != 0 {
				b.Fatalf("unexpected pruning: %+v", stats)
			}
			return
		}
		if !waitJournalCatchup(ctx, 50*time.Millisecond) {
			b.Fatalf("recovery deadline: %d/%d journal keys missing", missing, len(expected))
		}
	}
}
