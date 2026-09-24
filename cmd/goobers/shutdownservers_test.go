package main

import (
	"io"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

// blockUntilHandler answers a single request only once stop is closed, so a
// caller can hold a connection open for as long as it wants and then release
// it deterministically instead of racing time.Sleep against Shutdown's own
// internal poll interval (which grows up to 500ms between checks and would
// otherwise make a timing-based test flaky). arrived closes the moment the
// request lands, so a caller can wait for the connection to actually be
// in-flight before calling Shutdown.
func blockUntilHandler(stop <-chan struct{}, arrived chan<- struct{}) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-stop
		w.WriteHeader(http.StatusOK)
	})
}

// afterStop returns a channel that closes after d, for use as blockUntilHandler's stop.
func afterStop(d time.Duration) <-chan struct{} {
	stop := make(chan struct{})
	time.AfterFunc(d, func() { close(stop) })
	return stop
}

// startHeldServer starts an httpapi.Server serving handler and fires a
// request at it in the background, returning once that request has reached
// the handler.
func startHeldServer(t *testing.T, handler http.Handler, arrived <-chan struct{}) *httpapi.Server {
	t.Helper()
	srv, err := httpapi.NewServer("127.0.0.1:0", handler, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("construct server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	go func() {
		resp, err := http.Get(srv.Scheme() + "://" + srv.Address())
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-arrived
	return srv
}

// TestShutdownHTTPServersGivesEachServerItsOwnDeadline is #4571's regression
// test.
//
// Before the fix, cmd/goobers/up.go created ONE shutdownCtx and reused it for
// the sequential apiServer.Shutdown(shutdownCtx) and
// webhookServer.Shutdown(shutdownCtx) calls. apiServer here never finishes
// draining within the grace period (its one in-flight request blocks until
// the test cleans it up), so its Shutdown call is guaranteed to consume the
// entire deadline and return context.DeadlineExceeded — by the time it
// returns, a shared context is already expired.
//
// webhookServer's own in-flight request is still open at that point too (it
// only unblocks after webhookHoldFor, chosen to outlast apiServer's
// Shutdown call), so its Shutdown genuinely has draining left to do:
//   - With a shared, already-expired context (the pre-fix bug), it fails
//     immediately with context.DeadlineExceeded even though its own request
//     finishes comfortably before a FRESH grace period would have elapsed.
//   - With its own fresh context (the fix), it gets the full grace period
//     measured from its own call, which is enough to drain normally.
func TestShutdownHTTPServersGivesEachServerItsOwnDeadline(t *testing.T) {
	const grace = 300 * time.Millisecond
	// webhookHoldFor outlasts apiServer's Shutdown call (which always takes
	// exactly `grace`, since apiServer never unblocks on its own) by enough
	// margin that webhookServer's connection is still active when its own
	// Shutdown is invoked, but comfortably finishes within a second, fresh
	// `grace` window measured from that call.
	const webhookHoldFor = grace + 150*time.Millisecond

	apiStop := make(chan struct{}) // never closed during the test: apiServer never finishes draining on its own
	apiArrived := make(chan struct{})
	apiServer := startHeldServer(t, blockUntilHandler(apiStop, apiArrived), apiArrived)
	t.Cleanup(func() { close(apiStop) })

	webhookArrived := make(chan struct{})
	webhookServer := startHeldServer(t, blockUntilHandler(afterStop(webhookHoldFor), webhookArrived), webhookArrived)

	apiErr, webhookErr := shutdownHTTPServers(apiServer, webhookServer, grace)
	if apiErr == nil {
		t.Fatal("api shutdown error = nil, want context deadline exceeded — apiServer's request never finishes draining in this test")
	}
	if webhookErr != nil {
		t.Errorf("webhook shutdown error = %v, want nil — a fresh deadline measured from webhookServer's OWN Shutdown call "+
			"must not be starved by however long apiServer's Shutdown call took", webhookErr)
	}
}

// TestShutdownHTTPServersToleratesNilWebhookServer pins that a daemon
// without a webhook listener configured shuts down cleanly: webhookServer is
// nil whenever the webhook plane was never started.
func TestShutdownHTTPServersToleratesNilWebhookServer(t *testing.T) {
	arrived := make(chan struct{})
	apiServer := startHeldServer(t, blockUntilHandler(afterStop(10*time.Millisecond), arrived), arrived)

	apiErr, webhookErr := shutdownHTTPServers(apiServer, nil, time.Second)
	if apiErr != nil {
		t.Errorf("api shutdown error = %v, want nil", apiErr)
	}
	if webhookErr != nil {
		t.Errorf("webhook shutdown error = %v, want nil when no webhook server was passed", webhookErr)
	}
}
