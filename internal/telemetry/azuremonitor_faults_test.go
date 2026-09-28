package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/ApplicationInsights-Go/appinsights"
)

// Exercise production envelope encoding, gzip, HTTP and durable replay. DNS
// and timeout errors are injected at DialContext, without public DNS/network
// dependencies. This is a correctness test, not a 30-minute platform load test.
func TestAzureReplayNetworkFaultRecovery(t *testing.T) {
	for _, fault := range []string{"429", "503", "reset", "ambiguous", "dns", "timeout"} {
		t.Run(fault, func(t *testing.T) {
			var offline atomic.Bool
			offline.Store(true)
			receiver := &replayFaultReceiver{t: t, fault: fault, offline: &offline, copies: make(map[string]int)}
			server := httptest.NewServer(receiver)
			defer server.Close()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if offline.Load() && fault == "dns" {
					return nil, &net.DNSError{Err: "synthetic DNS failure", Name: "fixture.invalid", IsNotFound: true}
				}
				if offline.Load() && fault == "timeout" {
					return nil, &net.DNSError{Err: "synthetic DNS timeout", Name: "fixture.invalid", IsTimeout: true}
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			client, err := newAzureMonitorClient("InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+server.URL,
				&http.Client{Transport: transport, Timeout: time.Second}, false, azureReplayConfig{})
			if err != nil {
				t.Fatal(err)
			}
			spool := testAzureReplaySpool(t, t.TempDir(), time.Now())
			client.replay, spool.send = spool, client.sendPayload
			first := appinsights.NewTraceTelemetry("fixture-first", appinsights.Information)
			if err = client.export(t.Context(), []appinsights.Telemetry{first}); err != nil {
				t.Fatal(err)
			}
			if err = spool.drain(t.Context()); err == nil || !isCollectorUnreachable(err) {
				t.Fatalf("fault not classified as remote delivery failure: %v", err)
			}
			if got := spool.stats(); got.PendingRecords != 1 || got.Delivered != 0 || got.Retried == 0 {
				t.Fatalf("failed delivery was not retained/counted: %+v", got)
			}
			second := appinsights.NewTraceTelemetry("fixture-second", appinsights.Information)
			if err = client.export(t.Context(), []appinsights.Telemetry{second}); err != nil {
				t.Fatalf("continued durable admission failed: %v", err)
			}
			offline.Store(false)
			if err = spool.drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := spool.stats(); got.PendingRecords != 0 || got.Delivered != 2 || got.PrunedBytes != 0 || got.Malformed != 0 {
				t.Fatalf("recovery accounting: %+v", got)
			}
			receiver.mu.Lock()
			defer receiver.mu.Unlock()
			wantFirst := 1
			if fault == "ambiguous" {
				wantFirst = 2
			}
			if len(receiver.copies) != 2 || receiver.copies[first.Properties["goobers.telemetry.record_id"]] != wantFirst || receiver.copies[second.Properties["goobers.telemetry.record_id"]] != 1 {
				t.Fatalf("lost or changed stable identity across retry: copies=%v", receiver.copies)
			}
		})
	}
}

type replayFaultReceiver struct {
	t       *testing.T
	fault   string
	offline *atomic.Bool
	mu      sync.Mutex
	copies  map[string]int
}

func (s *replayFaultReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	failing := s.offline.Load()
	if !failing || s.fault == "ambiguous" {
		s.record(r)
	}
	if !failing {
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.fault == "reset" || s.fault == "ambiguous" {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			s.t.Error(err)
			return
		}
		_ = conn.Close() // Ambiguous case already persisted the received identity.
		return
	}
	if s.fault == "429" {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (s *replayFaultReceiver) record(r *http.Request) {
	reader, err := gzip.NewReader(r.Body)
	if err != nil {
		s.t.Error(err)
		return
	}
	defer func() { _ = reader.Close() }()
	decoder := json.NewDecoder(reader)
	for {
		var envelope struct {
			Data struct {
				BaseData struct{ Properties map[string]string }
			}
		}
		if err = decoder.Decode(&envelope); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			s.t.Error(err)
			return
		}
		id := envelope.Data.BaseData.Properties["goobers.telemetry.record_id"]
		if id == "" {
			s.t.Error("missing stable identity")
		}
		s.mu.Lock()
		s.copies[id]++
		s.mu.Unlock()
	}
}
