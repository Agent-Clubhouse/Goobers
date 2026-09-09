package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestPodAgentEventOpKeyPreservesLegacyIdentity(t *testing.T) {
	t.Setenv(dispatcher.EnvStage, "implement")
	t.Setenv(dispatcher.EnvPodAttempt, "")
	event := journal.Event{Type: journal.EventAgentLifecycle, Seq: 7}
	for i := 0; i < 2; i++ {
		if got := podAgentEventOpKey(event); got != "implement/agent.lifecycle/7" {
			t.Fatalf("legacy key=%q", got)
		}
	}
}

func TestPodAgentEventsSurviveEqualPayloadsAndLostAcknowledgements(t *testing.T) {
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{RunID: "agent-run", Gaggle: "test", Workflow: "workflow", WorkflowVersion: 1, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(g string) (string, bool) { return root, g == "test" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	var mu sync.Mutex
	deliveries := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Ops) != 1 {
			t.Errorf("decode append: %v, ops=%d", err, len(req.Ops))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		out, err := writer.Emit(r.Context(), req)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		deliveries[req.Ops[0].Key]++
		first := deliveries[req.Ops[0].Key] == 1
		mu.Unlock()
		if first {
			// The append committed, but the caller did not receive its ACK.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	for _, key := range dispatcher.DispatcherControlEnv {
		t.Setenv(key, "")
	}
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "agent-run")
	t.Setenv(dispatcher.EnvGaggle, "test")
	t.Setenv(dispatcher.EnvStage, "implement")
	t.Setenv(dispatcher.EnvPodAttempt, "3")
	at := time.Now().UTC()
	want := []journal.AgentLifecycle{journal.AgentStarted, journal.AgentWaiting, journal.AgentWaiting, journal.AgentCompleted}
	for _, lifecycle := range want {
		// Real adapters leave Event.Seq zero. The two waiting emissions are
		// deliberately byte-identical and are still separate observations.
		event := journal.Event{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "copilot-1", RunID: "agent-run", Stage: "implement", Attempt: 1,
			Lifecycle: lifecycle, StartedAt: at, UpdatedAt: at, Fidelity: journal.AgentFidelityFull,
		}}
		// Independent recorder values share the pod's event identity space.
		if err := (podArtifactRecorder{}).Append(event); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := journal.OpenRead(filepath.Join(root, "agent-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var got []journal.AgentLifecycle
	for _, event := range events {
		if event.Type == journal.EventAgentLifecycle {
			got = append(got, event.Agent.Lifecycle)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted lifecycle=%v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != len(want) {
		t.Fatalf("distinct operation keys=%d, want %d", len(deliveries), len(want))
	}
	for key, count := range deliveries {
		if count != 2 {
			t.Errorf("operation %q delivered %d times, want original plus retry", key, count)
		}
	}
}
