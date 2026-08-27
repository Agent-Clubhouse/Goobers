package hostedprogress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestEnvironmentRequiresActionsContract(t *testing.T) {
	for _, name := range []string{"GITHUB_REPOSITORY", "GITHUB_SHA", "GITHUB_RUN_ID", "GITHUB_TOKEN", "GH_TOKEN"} {
		t.Setenv(name, "")
	}
	_, err := Environment()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_REPOSITORY") ||
		!strings.Contains(err.Error(), "checks: write") {
		t.Fatalf("Environment() error = %v", err)
	}
}

func TestEnvironmentUsesGitHubDefaults(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "owner/repo")
	t.Setenv("GITHUB_SHA", "deadbeef")
	t.Setenv("GITHUB_RUN_ID", "123")
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GITHUB_SERVER_URL", "")
	env, err := Environment()
	if err != nil {
		t.Fatal(err)
	}
	if env.APIURL != "https://api.github.com" || env.ServerURL != "https://github.com" {
		t.Fatalf("Environment() URLs = %q, %q", env.APIURL, env.ServerURL)
	}
}

func TestPublisherCreatesUpdatesAndDeduplicatesCheckRun(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	defer server.Close()

	runDir, events := testJournal(t)
	publisher := New(GitHubEnvironment{
		Repository:   "owner/repo",
		SHA:          "deadbeef",
		ActionsRunID: "123",
		Token:        "token",
		APIURL:       server.URL,
		ServerURL:    "https://github.example",
	}, runDir)

	if err := publisher.Publish(context.Background(), events[:1]); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), events[:1]); err != nil {
		t.Fatal(err)
	}
	heartbeatEvents := append([]journal.Event(nil), events[:1]...)
	heartbeatEvents = append(heartbeatEvents, journal.Event{
		Seq:  events[0].Seq + 1,
		Type: journal.EventStageHeartbeat,
	})
	if err := publisher.Publish(context.Background(), heartbeatEvents); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if requests[0]["external_id"] != Schema+":0123456789abcdef0123456789abcdef:123" {
		t.Fatalf("external_id = %v", requests[0]["external_id"])
	}
	output := requests[0]["output"].(map[string]any)
	text := output["text"].(string)
	if !strings.Contains(text, startMarker) || !strings.Contains(text, `"schema":"`+Schema+`"`) {
		t.Fatalf("check output does not contain hosted progress contract: %s", text)
	}
	if requests[1]["status"] != "completed" || requests[1]["conclusion"] != "success" {
		t.Fatalf("terminal update = %#v", requests[1])
	}
}

func TestBoundContractPreservesLatestEvents(t *testing.T) {
	events := []journal.Event{{Seq: 1, Type: journal.EventRunStarted}}
	for i := 2; i < 1000; i++ {
		events = append(events, journal.Event{
			Seq:  uint64(i),
			Type: journal.EventError,
			Error: &journal.ErrorDetail{
				Code:    "large",
				Message: strings.Repeat("x", 200),
			},
		})
	}

	contract := Contract{Schema: Schema, Events: events}
	boundContract(&contract)
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("bounded payload = %d bytes", len(raw))
	}
	if contract.Events[0].Type != journal.EventRunStarted ||
		contract.Events[len(contract.Events)-1].Seq != 999 ||
		contract.TruncatedBefore == 0 {
		t.Fatalf("bounded events did not preserve start/latest: %#v", contract)
	}
}

func TestConclusionMapping(t *testing.T) {
	tests := map[journal.RunPhase]string{
		journal.PhaseCompleted: "success",
		journal.PhaseFailed:    "failure",
		journal.PhaseAborted:   "cancelled",
		journal.PhaseEscalated: "action_required",
	}
	for phase, want := range tests {
		if got := conclusion(phase); got != want {
			t.Errorf("conclusion(%q) = %q, want %q", phase, got, want)
		}
	}
}

func TestBoundContractCompactsSingleOversizedEvent(t *testing.T) {
	contract := Contract{
		Schema:   Schema,
		Revision: 1,
		Events: []journal.Event{{
			Seq:  1,
			Type: journal.EventError,
			Error: &journal.ErrorDetail{
				Code:    "large",
				Message: strings.Repeat("x", maxPayloadBytes*2),
			},
		}},
	}
	boundContract(&contract)
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxPayloadBytes || len(contract.Events) != 1 ||
		len(contract.Events[0].Error.Message) > 1027 {
		t.Fatalf("single event was not compacted: bytes=%d event=%#v", len(raw), contract.Events)
	}
}

func testJournal(t *testing.T) (string, []journal.Event) {
	t.Helper()
	runsDir := t.TempDir()
	runID := "0123456789abcdef0123456789abcdef"
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implement-locally",
		WorkflowVersion: 1,
		Gaggle:          "crawler",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       time.Now().UTC(),
	}, map[string][]byte{
		journal.PinnedWorkflowGraphInputName: []byte(`{"nodes":[{"id":"plan"}],"edges":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(runsDir, runID), events
}
