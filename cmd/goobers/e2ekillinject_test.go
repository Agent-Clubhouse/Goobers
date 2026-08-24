package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// fakeDaemonServer serves the two routes waitForRunningAttempt/
// waitForSuccessorAndCompletion poll, with handlers the test controls per
// call so polling behavior (nothing yet, then a match) is exercised without
// a real daemon.
func fakeDaemonServer(t *testing.T, attempts func() readservice.AttemptList, detail func() readservice.RunDetail) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runs/{run}/stages/{stage}/attempts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attempts())
	})
	mux.HandleFunc("/api/v1/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(detail())
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestWaitForRunningAttemptFindsPlacedRunningAttempt(t *testing.T) {
	var calls atomic.Int32
	server := fakeDaemonServer(t, func() readservice.AttemptList {
		n := calls.Add(1)
		if n < 2 {
			// First poll: nothing running yet.
			return readservice.AttemptList{RunID: "run-1", Stage: "probe-builtin", Attempts: []readservice.StageAttempt{
				{ID: "a1", Status: "queued"},
			}}
		}
		return readservice.AttemptList{RunID: "run-1", Stage: "probe-builtin", Attempts: []readservice.StageAttempt{
			{ID: "a1", Status: "running", Placement: &journal.Placement{Pod: "probe-builtin-a1"}},
		}}
	}, nil)

	client := &e2eDaemonClient{baseURL: server.URL, http: server.Client()}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	got, err := waitForRunningAttempt(context.Background(), client, "run-1", "probe-builtin", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRunningAttempt: %v", err)
	}
	if got.ID != "a1" || got.Placement == nil || got.Placement.Pod != "probe-builtin-a1" {
		t.Fatalf("got = %+v, want the running attempt with pod placement", got)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 polls (queued then running), got %d", calls.Load())
	}
}

func TestWaitForRunningAttemptTimesOutWithClearError(t *testing.T) {
	server := fakeDaemonServer(t, func() readservice.AttemptList {
		return readservice.AttemptList{RunID: "run-1", Stage: "probe-builtin", Attempts: nil}
	}, nil)
	client := &e2eDaemonClient{baseURL: server.URL, http: server.Client()}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	_, err := waitForRunningAttempt(context.Background(), client, "run-1", "probe-builtin", 0)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
}

func TestWaitForSuccessorAndCompletionObservesSuccessorAndTerminal(t *testing.T) {
	interrupted := readservice.StageAttempt{ID: "a1", Placement: &journal.Placement{Pod: "probe-builtin-a1"}}
	var calls atomic.Int32
	server := fakeDaemonServer(t, func() readservice.AttemptList {
		return readservice.AttemptList{Attempts: []readservice.StageAttempt{
			interrupted,
			{ID: "a2", Placement: &journal.Placement{Pod: "probe-builtin-a2"}},
		}}
	}, func() readservice.RunDetail {
		n := calls.Add(1)
		terminal := n >= 2
		return readservice.RunDetail{RunSummary: readservice.RunSummary{Terminal: terminal, Phase: journal.PhaseCompleted}}
	})
	client := &e2eDaemonClient{baseURL: server.URL, http: server.Client()}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	successor, completed, err := waitForSuccessorAndCompletion(context.Background(), client, "run-1", "probe-builtin", interrupted, 5*time.Second)
	if err != nil {
		t.Fatalf("waitForSuccessorAndCompletion: %v", err)
	}
	if successor == nil || successor.ID != "a2" {
		t.Fatalf("successor = %+v, want attempt a2", successor)
	}
	if !completed {
		t.Fatal("expected the run to be classified as completed")
	}
}

func TestWaitForSuccessorAndCompletionTimesOutWithPartialResult(t *testing.T) {
	interrupted := readservice.StageAttempt{ID: "a1", Placement: &journal.Placement{Pod: "probe-builtin-a1"}}
	server := fakeDaemonServer(t, func() readservice.AttemptList {
		return readservice.AttemptList{Attempts: []readservice.StageAttempt{interrupted}}
	}, func() readservice.RunDetail {
		return readservice.RunDetail{RunSummary: readservice.RunSummary{Terminal: false}}
	})
	client := &e2eDaemonClient{baseURL: server.URL, http: server.Client()}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	successor, completed, err := waitForSuccessorAndCompletion(context.Background(), client, "run-1", "probe-builtin", interrupted, 0)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if successor != nil {
		t.Fatalf("successor = %+v, want nil (none ever appeared)", successor)
	}
	if completed {
		t.Fatal("a timed-out wait must not report the run as completed")
	}
}
