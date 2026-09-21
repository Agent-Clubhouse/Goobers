package telemetry

import (
	"context"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// DiagnosticBatchLimit matches the bounded reference receiver contract.
	DiagnosticBatchLimit      = 128
	DiagnosticRequestLimit    = 1 << 20
	DiagnosticQueuedByteLimit = 8 << 20
)

type diagnosticBatch struct {
	request *collectorlogpb.ExportLogsServiceRequest
	bytes   int
	count   uint64
}

// EmitBatch snapshots at most 128 records without waiting for the collector.
// Invalid input rejects the whole batch; valid records are split into <=1 MiB
// requests. Queue limits are 128 requests AND 8 MiB encoded bytes, plus one
// in-flight request. Accepted/Delivered/Dropped count records; Failures counts failed or
// partially rejected RPCs.
func (d *DiagnosticExporter) EmitBatch(records []DiagnosticRecord) int {
	if d == nil || len(records) == 0 {
		return 0
	}
	if len(records) > DiagnosticBatchLimit {
		d.dropped.Add(uint64(len(records)))
		return 0
	}
	entries := make([]*logpb.LogRecord, 0, len(records))
	for _, record := range records {
		entry := d.diagnosticEntry(record)
		if entry == nil {
			d.dropped.Add(uint64(len(records)))
			return 0
		}
		entries = append(entries, entry)
	}
	accepted := 0
	for len(entries) > 0 {
		count := len(entries)
		request := d.diagnosticRequest(entries[:count])
		for proto.Size(request) > DiagnosticRequestLimit {
			count /= 2
			request = d.diagnosticRequest(entries[:count])
		}
		if d.enqueueBatch(diagnosticBatch{request: request, bytes: proto.Size(request), count: uint64(count)}) {
			accepted += count
		}
		entries = entries[count:]
	}
	return accepted
}

func (d *DiagnosticExporter) enqueueBatch(batch diagnosticBatch) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed && d.queuedBytes+batch.bytes <= DiagnosticQueuedByteLimit {
		select {
		case d.queue <- batch:
			d.queuedBytes += batch.bytes
			d.accepted.Add(batch.count)
			return true
		default:
		}
	}
	d.dropped.Add(batch.count)
	return false
}

func (d *DiagnosticExporter) exportBatch(batch diagnosticBatch) {
	if d.ctx.Err() != nil {
		d.dropped.Add(batch.count)
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, diagnosticExportTimeout)
	response, err := d.client.Export(ctx, batch.request)
	cancel()
	if err != nil {
		d.failures.Add(1)
		d.dropped.Add(batch.count)
		return
	}
	rejected := response.GetPartialSuccess().GetRejectedLogRecords()
	if rejected < 0 || uint64(rejected) > batch.count {
		// A malformed acknowledgement cannot establish any delivery.
		rejected = int64(batch.count)
	}
	if rejected > 0 {
		d.failures.Add(1)
		d.dropped.Add(uint64(rejected))
	}
	d.delivered.Add(batch.count - uint64(rejected))
}
