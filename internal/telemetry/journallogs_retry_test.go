package telemetry

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/goobers/goobers/internal/journal"
)

type rejectingJournalLogsReceiver struct {
	collectorlogspb.UnimplementedLogsServiceServer
	calls    atomic.Uint64
	response *collectorlogspb.ExportLogsServiceResponse
	err      error
}

func (s *rejectingJournalLogsReceiver) Export(context.Context, *collectorlogspb.ExportLogsServiceRequest) (*collectorlogspb.ExportLogsServiceResponse, error) {
	s.calls.Add(1)
	return s.response, s.err
}

func TestJournalLogsDoesNotRetryAmbiguousExportFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *collectorlogspb.ExportLogsServiceResponse
		err      error
	}{
		{name: "unavailable", err: status.Error(codes.Unavailable, "response lost")},
		{name: "deadline-exceeded", err: status.Error(codes.DeadlineExceeded, "response late")},
		{
			name: "partial-success",
			response: &collectorlogspb.ExportLogsServiceResponse{
				PartialSuccess: &collectorlogspb.ExportLogsPartialSuccess{
					RejectedLogRecords: 1, ErrorMessage: "record rejected",
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			receiver := &rejectingJournalLogsReceiver{response: test.response, err: test.err}
			collectorlogspb.RegisterLogsServiceServer(server, receiver)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			client, err := New(context.Background(), Config{
				JournalLogsOnly: true, JournalLogs: true, Exporter: ExporterOTLP,
				OTLPEndpoint: listener.Addr().String(), OTLPInsecure: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = client.Shutdown(ctx)
			})
			client.Commit(journal.CommittedEvent{JournalID: "test-journal", Kind: "run", Body: []byte("{}")})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.journalLogs.flush(ctx); err != nil {
				t.Fatalf("export did not finish after its first failed RPC: %v", err)
			}
			if err := client.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if got := receiver.calls.Load(); got != 1 {
				t.Fatalf("physical export RPCs = %d, want exactly one attempt", got)
			}
			stats := client.JournalExportStats()
			if stats.Accepted != 1 || stats.ExportFailures != 1 || stats.Dropped != 0 {
				t.Fatalf("queue acceptance was mistaken for backend receipt: %+v", stats)
			}
		})
	}
}
