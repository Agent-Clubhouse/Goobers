package httpapi

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func budgetTestServer(t *testing.T, budget time.Duration, handler http.HandlerFunc, logs *bytes.Buffer) *httptest.Server {
	t.Helper()
	logger := log.New(logs, "", 0)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWithBudgetAnswer(logger, "testRoute", budget, w, r, handler, func() {})
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestBlockedHandlerGets503NotAStreamReset(t *testing.T) {
	var logs bytes.Buffer
	release := make(chan struct{})
	defer close(release)
	server := budgetTestServer(t, 50*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		<-release // blocked on a lock; never observes its context
		w.Header().Set("X-Late", "1")
		_, _ = io.WriteString(w, "late")
	}, &logs)
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("a blocked handler must answer, not reset the stream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "request_budget_exceeded") ||
		response.Header.Get("Retry-After") == "" || response.Header.Get("X-Late") != "" {
		t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, raw)
	}
	if !strings.Contains(logs.String(), "route=testRoute") || !strings.Contains(logs.String(), "budget exceeded") {
		t.Fatalf("budget expiry was not logged with its route: %q", logs.String())
	}
}

func TestLateHandlerWriteAfterPreemptionIsDiscarded(t *testing.T) {
	var logs bytes.Buffer
	done := make(chan error, 1)
	server := budgetTestServer(t, 30*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(120 * time.Millisecond)
		_, err := io.WriteString(w, "late")
		done <- err
	}, &logs)
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if err := <-done; err == nil {
		t.Fatal("late write after the budget answer must fail so the handler stops")
	}
}

func TestStartedResponseIsNotPreempted(t *testing.T) {
	var logs bytes.Buffer
	server := budgetTestServer(t, 30*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Mine", "1")
		w.WriteHeader(http.StatusAccepted)
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	}, &logs)
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusAccepted || string(raw) != "done" || response.Header.Get("X-Mine") != "1" || logs.Len() != 0 {
		t.Fatalf("status=%d body=%q logs=%q", response.StatusCode, raw, logs.String())
	}
}

func TestImplicitOKKeepsHandlerHeaders(t *testing.T) {
	var logs bytes.Buffer
	server := budgetTestServer(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Only", "header")
	}, &logs)
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Only") != "header" {
		t.Fatalf("status=%d headers=%v", response.StatusCode, response.Header)
	}
}

func TestAbandonedHandlerKeepsItsSlotUntilItReturns(t *testing.T) {
	var logs bytes.Buffer
	release := make(chan struct{})
	slotReleased := make(chan struct{})
	logger := log.New(&logs, "", 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWithBudgetAnswer(logger, "testRoute", 30*time.Millisecond, w, r, func(http.ResponseWriter, *http.Request) {
			<-release
		}, func() { close(slotReleased) })
	}))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}
	select {
	case <-slotReleased:
		t.Fatal("the slot was returned while the abandoned handler still ran")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-slotReleased:
	case <-time.After(5 * time.Second):
		t.Fatal("the slot was never returned after the handler ended")
	}
}

func TestHandlerPanicStillReachesNetHTTP(t *testing.T) {
	var logs bytes.Buffer
	server := budgetTestServer(t, time.Second, func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}, &logs)
	if _, err := server.Client().Get(server.URL); err == nil {
		t.Fatal("an aborted handler must still abort the response")
	}
}
