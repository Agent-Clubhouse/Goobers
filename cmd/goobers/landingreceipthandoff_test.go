package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/providers"
)

func TestStageLandingHandoffRequiresDurableHostAck(t *testing.T) {
	workspace, runs := t.TempDir(), t.TempDir()
	t.Chdir(workspace)
	const runID = "landing-stage"
	run, err := journal.Create(runs, journal.RunIdentity{RunID: runID, Gaggle: "web", Workflow: "landing", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runs, gaggle == "web" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	var refuse atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer journal-only-token" || r.URL.Path != "/api/v1/runs/"+runID+"/journal/emit" {
			t.Error("receipt handoff used the wrong credential or run")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if refuse.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var request livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		response, err := writer.Emit(r.Context(), request)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv(journalclient.EnvEndpoint, server.URL)
	t.Setenv(journalclient.EnvToken, "journal-only-token")
	t.Setenv(journalclient.EnvRunID, runID)
	t.Setenv(journalclient.EnvGaggle, "web")
	t.Setenv("GOOBERS_POD_TOKEN", "surrender-token-must-not-be-used")
	recorder := sidecarMutationRecorder{kind: "pr"}
	intent := providers.LandingIntent{ID: "intent-42", Operation: "merge", RepositoryAPIURL: "https://api.github.com/repos/acme/app", PullID: "42"}
	refuse.Store(true)
	if err := recorder.RecordLandingIntent(t.Context(), providers.ProviderGitHub, intent); err == nil {
		t.Fatal("refused host acknowledgement allowed a landing intent to succeed")
	}
	before, err := os.ReadFile(filepath.Join(workspace, mutationsSidecarFile))
	if err != nil {
		t.Fatal(err)
	}
	refuse.Store(false)
	for range 2 {
		if err := publishStageLandingReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := recorder.RecordLandingReceipt(cancelled, providers.ExternalRef{Provider: providers.ProviderGitHub, Ref: "github#42", Operation: "merge", LandingIntent: &intent}); err != nil {
		t.Fatalf("completed mutation lost its receipt to request cancellation: %v", err)
	}
	writer.Close()
	reader, err := journal.OpenReadOnly(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerMutationRecovered {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("host retained %d receipts, want one intent and one result despite retries", count)
	}
	after, err := os.ReadFile(filepath.Join(workspace, mutationsSidecarFile))
	if err != nil || len(after) <= len(before) || string(after[:len(before)]) != string(before) {
		t.Fatal("handoff discarded or rewrote local retry evidence")
	}
}

func TestUnacknowledgedLandingIntentNeverReachesForge(t *testing.T) {
	for _, mode := range []string{"refused", "invalid-ack", "missing-endpoint", "missing-token", "missing-run", "missing-gaggle"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var forgeCalls atomic.Int32
			forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				forgeCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(forge.Close)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "invalid-ack" {
					_ = json.NewEncoder(w).Encode(livejournal.EmitResponse{})
					return
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			t.Cleanup(host.Close)
			values := map[string]string{journalclient.EnvEndpoint: host.URL, journalclient.EnvToken: "journal-only-token", journalclient.EnvRunID: "landing-stage", journalclient.EnvGaggle: "web"}
			missing := map[string]string{"missing-endpoint": journalclient.EnvEndpoint, "missing-token": journalclient.EnvToken, "missing-run": journalclient.EnvRunID, "missing-gaggle": journalclient.EnvGaggle}
			if key := missing[mode]; key != "" {
				values[key] = ""
			}
			for key, value := range values {
				t.Setenv(key, value)
			}
			provider := providers.NewGitHubProvider("forge-token", providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
			provider.BaseURL = forge.URL
			_, err := provider.MergePullRequest(t.Context(), providers.MergePullRequestRequest{Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}, PullID: "42"})
			if err == nil || forgeCalls.Load() != 0 {
				t.Fatalf("unacknowledged intent reached forge: %v calls=%d", err, forgeCalls.Load())
			}
			if data, err := os.ReadFile(mutationsSidecarFile); err != nil || len(data) == 0 {
				t.Fatal("refused handoff lost its local intent evidence")
			}
		})
	}
}
