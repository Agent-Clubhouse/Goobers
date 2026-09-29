package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestStartupResponseProbeRetainsHTTPAndTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/failure" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	ready := time.Now()
	for path, status := range map[string]int{"/readyz": 200, "/failure": 503} {
		probe := startupResponseProbe(t.Context(), server.Client(), server.URL, path, ready)
		if probe.Status != status || probe.Error != "" || probe.LatencyMS <= 0 || probe.StartedAfterReadyMS < 0 {
			t.Fatalf("invalid measured response: %+v", probe)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	probe := startupResponseProbe(ctx, server.Client(), server.URL, "/readyz", ready)
	if probe.Error == "" || probe.Status != 0 {
		t.Fatalf("transport failure was hidden: %+v", probe)
	}
}

func TestStartupResponsivenessRequiresRealSuccessfulWorkAndProbes(t *testing.T) {
	good := startupResponsiveness{FirstWorkflowCommandMS: 1, SourceRunEvents: 1, SourceRuns: 1, SourceCompletedRuns: 1,
		Probes: []startupHTTPProbe{{Path: "/readyz", Status: 200}, {Path: "/api/v1/instance", Status: 200}}}
	if !good.Successful() || good.DeliveryReconciled {
		t.Fatal("success incorrectly requires or claims delivery reconciliation")
	}
	for _, change := range []func(*startupResponsiveness){
		func(r *startupResponsiveness) { r.FirstWorkflowError = "failed" },
		func(r *startupResponsiveness) { r.FirstWorkflowCommandMS = 0 },
		func(r *startupResponsiveness) { r.SourceRunEvents = 0 },
		func(r *startupResponsiveness) { r.SourceRuns = 2 },
		func(r *startupResponsiveness) { r.SourceCompletedRuns = 0 },
		func(r *startupResponsiveness) { r.SourceJournalError = "invalid" },
		func(r *startupResponsiveness) { r.Probes = nil },
		func(r *startupResponsiveness) { r.Probes[0].Status = 503 },
		func(r *startupResponsiveness) { r.Probes[0].Error = "timeout" },
	} {
		bad := good
		bad.Probes = append([]startupHTTPProbe(nil), good.Probes...)
		change(&bad)
		if bad.Successful() {
			t.Fatalf("incomplete/failed measurement accepted: %+v", bad)
		}
	}
}

func TestStartupWorkflowJournalRequiresConsecutiveCompletedRun(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"complete", "{\"seq\":1,\"type\":\"run.started\"}\n{\"seq\":2,\"type\":\"run.finished\",\"status\":\"completed\"}\n", true},
		{"gap", "{\"seq\":2,\"type\":\"run.finished\",\"status\":\"completed\"}\n", false},
		{"unfinished", "{\"seq\":1,\"type\":\"run.started\"}\n", false},
		{"failed", "{\"seq\":1,\"type\":\"run.finished\",\"status\":\"failed\"}\n", false},
		{"corrupt", "not-json\n", false},
		{"two-terminals", "{\"seq\":1,\"type\":\"run.finished\",\"status\":\"completed\"}\n{\"seq\":2,\"type\":\"run.finished\",\"status\":\"completed\"}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(filepath.Join(root, "gaggles/demo/runs/test/events.jsonl"), tc.body)
			events, runs, completed, err := startupWorkflowJournals(root)
			if tc.valid {
				if err != "" || events != 2 || runs != 1 || completed != 1 {
					t.Fatalf("valid journal rejected: %d %d %d %s", events, runs, completed, err)
				}
			} else if err == "" {
				t.Fatal("invalid journal accepted")
			}
		})
	}
}
