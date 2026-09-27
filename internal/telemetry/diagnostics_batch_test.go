package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func TestDiagnosticBatchFleetBurstDelivery(t *testing.T) {
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 32), headers: make(chan metadata.MD, 32)}
	d, err := NewDiagnosticExporter(Config{OTLPEndpoint: startDiagnosticTestCollector(t, c), OTLPInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	// 100 gaggles, one deployment heartbeat, eleven features per gaggle.
	remaining := 1201
	for remaining > 0 {
		count := min(remaining, DiagnosticBatchLimit)
		records := make([]DiagnosticRecord, count)
		for i := range records {
			records[i] = diagnosticTestRecord()
		}
		if got := d.EmitBatch(records); got != count {
			t.Fatalf("accepted %d/%d", got, count)
		}
		remaining -= count
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := d.Stats(); stats.Accepted != 1201 || stats.Delivered != 1201 || stats.Dropped != 0 {
		t.Fatal(stats)
	}
	if len(c.requests) != 10 {
		t.Fatalf("expected ten bounded RPCs, got %d", len(c.requests))
	}
}

func TestDiagnosticBatchSplitsByBytesAndAccountsPartialRejection(t *testing.T) {
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 32), headers: make(chan metadata.MD, 32), reject: true}
	d, err := NewDiagnosticExporter(Config{OTLPEndpoint: startDiagnosticTestCollector(t, c), OTLPInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	records := make([]DiagnosticRecord, 40)
	for i := range records {
		records[i] = diagnosticTestRecord()
		records[i].Attributes["bounded"] = strings.Repeat("x", 48<<10)
	}
	if d.EmitBatch(records) != len(records) {
		t.Fatal("valid batch rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	requests := len(c.requests)
	if requests < 2 {
		t.Fatal("oversized request not split")
	}
	for i := 0; i < requests; i++ {
		request := <-c.requests
		if proto.Size(request) > DiagnosticRequestLimit {
			t.Fatal("request exceeds receiver byte bound")
		}
	}
	stats := d.Stats()
	if stats.Accepted != 40 || stats.Dropped != uint64(requests) || stats.Delivered != 40-uint64(requests) || stats.Failures != uint64(requests) {
		t.Fatal(stats)
	}
}

func TestDiagnosticBatchBytePressureAndCancellation(t *testing.T) {
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 1), headers: make(chan metadata.MD, 1), block: true}
	d, err := NewDiagnosticExporter(Config{OTLPEndpoint: startDiagnosticTestCollector(t, c), OTLPInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	d.Emit(diagnosticTestRecord())
	select {
	case <-c.requests:
	case <-time.After(5 * time.Second):
		t.Fatal("no export")
	}
	records := make([]DiagnosticRecord, 16)
	for i := range records {
		records[i] = diagnosticTestRecord()
		records[i].Attributes["bounded"] = strings.Repeat("x", 48<<10)
	}
	for i := 0; i < 20; i++ {
		d.EmitBatch(records)
	}
	d.mu.Lock()
	queued, requests := d.queuedBytes, len(d.queue)
	d.mu.Unlock()
	if queued > DiagnosticQueuedByteLimit || requests >= DiagnosticQueueLimit || d.Stats().Dropped == 0 {
		t.Fatalf("byte budget ineffective: bytes=%d requests=%d stats=%+v", queued, requests, d.Stats())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d.Shutdown(ctx) == nil {
		t.Fatal("expected cancellation")
	}
	stats := d.Stats()
	if stats.Delivered != 0 || stats.Dropped != 321 || d.queuedBytes != 0 {
		t.Fatal(stats, d.queuedBytes)
	}
}

func TestDiagnosticBatchInvalidInputRejectsWholeBatch(t *testing.T) {
	d := &DiagnosticExporter{queue: make(chan diagnosticBatch, DiagnosticQueueLimit)}
	valid := diagnosticTestRecord()
	invalid := diagnosticTestRecord()
	invalid.Attributes["private"] = map[string]any{"prompt": "not allowed"}
	if d.EmitBatch([]DiagnosticRecord{valid, invalid}) != 0 || len(d.queue) != 0 || d.Stats().Dropped != 2 {
		t.Fatal("invalid batch partially admitted", d.Stats())
	}
	tooMany := make([]DiagnosticRecord, DiagnosticBatchLimit+1)
	if d.EmitBatch(tooMany) != 0 || d.Stats().Dropped != uint64(len(tooMany)+2) {
		t.Fatal("batch record bound not enforced", d.Stats())
	}
}
