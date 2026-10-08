package telemetry

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/microsoft/ApplicationInsights-Go/appinsights"
)

func TestClassifyAzureDeliveryVocabulary(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil":       {nil, ""},
		"rejected":  {&azureMonitorDeliveryError{cause: &azureMonitorRejectedError{status: http.StatusBadRequest}}, azureDeliveryRejected},
		"partial":   {&azureMonitorRejectedError{status: http.StatusPartialContent, partial: true}, azureDeliveryRejected},
		"malformed": {&azureMonitorMalformedError{cause: errors.New("bad envelope")}, azureDeliveryMalformed},
		"deadline":  {context.DeadlineExceeded, azureDeliveryTimeout},
		"dns":       {&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}}, azureDeliveryDNS},
		// A DNS lookup that timed out is a timeout, not a DNS answer.
		"dns timeout": {&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}}, azureDeliveryTimeout},
		"refused":     {&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, azureDeliveryNetwork},
		"eof":         {&url.Error{Op: "Post", Err: io.EOF}, azureDeliveryNetwork},
		"unknown":     {errors.New("opaque"), azureDeliveryUnknown},
	} {
		if got := classifyAzureDelivery(tc.err); got != tc.want {
			t.Errorf("%s: class = %q, want %q", name, got, tc.want)
		}
	}
}

// A malformed file or refused admission during an outage must not relabel the
// active failure, and a readable manifest clears an active spool_io failure.
func TestAzureDeliveryStateKeepsActiveClass(t *testing.T) {
	var state azureDeliveryState
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	state.failed(at, azureDeliveryTLS, true)
	state.failed(at.Add(time.Second), azureDeliveryMalformed, false)
	state.spoolReadable()
	if status, active := state.snapshot(); !active || status.FailureClass != azureDeliveryTLS || !status.LastFailure.Equal(at) {
		t.Fatalf("active TLS failure relabelled: %+v active=%v", status, active)
	}
	state.succeeded(at.Add(2 * time.Second))
	state.failed(at.Add(3*time.Second), azureDeliverySpoolIO, true)
	state.spoolReadable()
	if status, active := state.snapshot(); active || status.FailureClass != azureDeliverySpoolIO {
		t.Fatalf("spool_io not cleared by a readable manifest: %+v active=%v", status, active)
	}
}

// An ingestion endpoint presenting a certificate the client does not trust is
// a TLS failure, distinct from DNS, timeout and plain network errors.
func TestAzureReplayDeliveryClassifiesTLSFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	client, err := newAzureMonitorClient("InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+server.URL,
		&http.Client{Timeout: time.Second}, false, azureReplayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	spool := testAzureReplaySpool(t, t.TempDir(), time.Now())
	client.replay, spool.send = spool, client.sendPayload
	if err = client.export(t.Context(), []appinsights.Telemetry{appinsights.NewTraceTelemetry("fixture", appinsights.Information)}); err != nil {
		t.Fatal(err)
	}
	if err = spool.drain(t.Context()); err == nil {
		t.Fatal("untrusted certificate delivered")
	}
	if got := spool.stats(); !got.ActiveFailure || got.FailureClass != azureDeliveryTLS {
		t.Fatalf("TLS failure evidence = %+v", got)
	}
}

// A malformed spool file is loss evidence with its own class. It does not hold
// the stream in an active failure: later records still deliver.
func TestAzureReplayDeliveryMalformedSpoolIsDistinctAndNotActive(t *testing.T) {
	dir := t.TempDir()
	spool := testAzureReplaySpool(t, dir, time.Now())
	spool.send = func(context.Context, []byte) error { return nil }
	// Present before the manifest is first opened, as after a crash.
	if err := os.WriteFile(filepath.Join(dir, "00000000000000000000-poison.ndjson"), []byte("not a replay file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spool.submit(t.Context(), []byte("{\"ok\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if err := spool.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := spool.stats()
	if got.Malformed == 0 || got.FailureClass != azureDeliveryMalformed || got.ActiveFailure || got.LastSuccess.IsZero() {
		t.Fatalf("malformed evidence = %+v", got)
	}
}

func TestAzureReplayHealthReportsDeliveryFailureAndRecovery(t *testing.T) {
	now := time.Now()
	state := replayHealthState{}
	failedAt := now.Add(-time.Second)
	stats := AzureReplayStats{AccountingReady: true, Accepted: 1, Retried: 1, LastFailure: failedAt, FailureClass: azureDeliveryTimeout, ActiveFailure: true}
	got := state.sample(now, stats, replayLossCounters{}, 10000)
	if got == nil || got.Status != "warning" || !slices.Contains(got.Causes, "delivery_failure") ||
		got.FailureClass != azureDeliveryTimeout || !got.ActiveFailure || got.LastFailure == nil || got.LastSuccess != nil {
		t.Fatalf("active failure warning = %+v", got)
	}
	stats.ActiveFailure, stats.Delivered, stats.LastSuccess = false, 1, now.Add(5*time.Second)
	got = state.sample(now.Add(10*time.Second), stats, replayLossCounters{}, 10000)
	if got == nil || got.Status != "recovered" || got.ActiveFailure || got.LastSuccess == nil || !got.LastSuccess.Equal(stats.LastSuccess) {
		t.Fatalf("recovery = %+v", got)
	}
}

// Last-success, last-failure and class survive a restart in a private,
// bounded sidecar; the active-failure flag does not.
//
// This asserts what a completed shutdown persists, not how quickly shutdown
// finishes (#6740). Each close therefore runs to completion rather than under
// a wall-clock deadline that slow file I/O can exhaust, and the first spool is
// closed only after its manifest initialization has finished so close does
// not race the initializer. A completed close also releases the manifest
// before TempDir cleanup.
func TestAzureReplayDeliveryStatusSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20}
	first, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// A first attempt that succeeded makes the manifest ready immediately; a
	// failed one would retry indefinitely, so fail rather than wait.
	<-first.index.firstAttempt
	if first.index.firstErr != nil {
		t.Fatal(first.index.firstErr)
	}
	<-first.index.ready
	success := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	first.delivery.succeeded(success)
	first.delivery.failed(success.Add(time.Minute), azureDeliveryRejected, true)
	if err = first.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "status-journal.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("status sidecar mode = %v", info.Mode().Perm())
	}
	if raw, _ := os.ReadFile(path); len(raw) > azureDeliveryStatusLimit || strings.Contains(string(raw), "http") {
		t.Fatalf("status sidecar is unbounded or carries an endpoint: %s", raw)
	}

	second, err := newAzureReplaySpool(cfg, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.close(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	got := second.stats()
	if !got.LastSuccess.Equal(success) || !got.LastFailure.Equal(success.Add(time.Minute)) ||
		got.FailureClass != azureDeliveryRejected || got.ActiveFailure {
		t.Fatalf("restored delivery evidence = %+v", got)
	}
	inspected := InspectAzureReplayRoot(root)
	if !inspected.LastSuccess.Equal(success) || inspected.FailureClass != azureDeliveryRejected {
		t.Fatalf("aggregate delivery evidence = %+v", inspected)
	}
}

func TestAzureDeliveryStatusRejectsOversizeAndUnknownClass(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "status-traces.json")
	if err := os.WriteFile(path, []byte(`{"schema":"`+azureDeliveryStatusSchema+`","failureClass":"https://private.example/"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := readAzureDeliveryStatus(root, "traces")
	if err != nil || status.FailureClass != azureDeliveryUnknown {
		t.Fatalf("foreign class = %+v, %v", status, err)
	}
	if err = os.WriteFile(path, []byte(strings.Repeat(" ", azureDeliveryStatusLimit+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = readAzureDeliveryStatus(root, "traces"); err == nil {
		t.Fatal("oversize status accepted")
	}
	if _, err = readAzureDeliveryStatus(root, "../escape"); err == nil {
		t.Fatal("invalid stream accepted")
	}
}

// Local storage refusing admission is spool_io evidence, not a destination
// failure: it does not mark the delivery path active.
func TestAzureReplayDeliveryAdmissionFailureIsSpoolIO(t *testing.T) {
	spool := testAzureReplaySpool(t, t.TempDir(), time.Now())
	spool.cfg.maxBytes = 1
	if err := spool.submit(t.Context(), []byte("{\"ok\":1}\n")); err == nil {
		t.Fatal("admission past the spool byte bound succeeded")
	}
	if got := spool.stats(); got.FailureClass != azureDeliverySpoolIO || got.ActiveFailure || got.AdmissionFailures != 1 {
		t.Fatalf("admission failure evidence = %+v", got)
	}
}
