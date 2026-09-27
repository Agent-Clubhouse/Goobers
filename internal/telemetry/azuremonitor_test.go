package telemetry

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestParseAzureMonitorConnectionString(t *testing.T) {
	tests := []struct {
		name, raw, endpoint, wantError string
	}{
		{
			name: "explicit endpoint",
			raw: "InstrumentationKey=00000000-0000-0000-0000-000000000000;" +
				"IngestionEndpoint=https://westus-0.in.applicationinsights.azure.com/",
			endpoint: "https://westus-0.in.applicationinsights.azure.com/v2.1/track",
		},
		{name: "default endpoint", raw: "InstrumentationKey=key", endpoint: "https://dc.services.visualstudio.com/v2.1/track"},
		{name: "missing key", raw: "IngestionEndpoint=https://example.test", wantError: "InstrumentationKey"},
		{name: "remote plaintext", raw: "InstrumentationKey=key;IngestionEndpoint=http://example.test", wantError: "HTTPS"},
		{name: "userinfo", raw: "InstrumentationKey=key;IngestionEndpoint=https://user@example.test", wantError: "invalid"},
		{name: "duplicate", raw: "InstrumentationKey=one;InstrumentationKey=two", wantError: "repeated"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAzureMonitorConnectionString(tc.raw)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ingestionURL != tc.endpoint {
				t.Fatalf("ingestion URL = %q, want %q", got.ingestionURL, tc.endpoint)
			}
		})
	}
}

func TestAzureMonitorExporterSendsCorrelatedScrubbedSpan(t *testing.T) {
	payloads := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2.1/track" {
			t.Errorf("path = %q", r.URL.Path)
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip reader: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		payloads <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"itemsReceived":1,"itemsAccepted":1,"errors":[]}`)
	}))
	defer server.Close()

	const secret = "tenant-secret-value"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	client, err := New(context.Background(), Config{
		ServiceName: "goobers", ServiceVersion: "v0.5.0-test", BuildCommit: "abc123",
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorHTTPClient:       server.Client(), Scrubber: scrubber,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "tenant-gaggle", WorkflowID: "poll", RunID: "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Succeed("completed " + secret)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	select {
	case body := <-payloads:
		for _, want := range []string{"tenant-gaggle", "poll", "0af7651916cd43dd8448eb211c80319c", "v0.5.0-test", "abc123"} {
			if !strings.Contains(body, want) {
				t.Errorf("payload missing %q: %s", want, body)
			}
		}
		if strings.Contains(body, secret) {
			t.Fatalf("payload contains registered secret: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Application Insights request")
	}
}
