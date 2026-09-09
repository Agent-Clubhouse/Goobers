package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestPodAgentProjectionsUseNewestPhysicalVisitDespiteLateDelivery(t *testing.T) {
	for _, previousLogical := range []int{1, 2} {
		t.Run("previous-logical-"+strconv.Itoa(previousLogical), func(t *testing.T) {
			root, reader := podAgentProjectionFixture(t)
			at := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
			emit := func(stage, pod string, logical int, id string, lifecycle journal.AgentLifecycle, cost int64, started, updated time.Time) {
				t.Helper()
				t.Setenv(dispatcher.EnvStage, stage)
				t.Setenv(dispatcher.EnvPodAttempt, pod)
				event := journal.Event{Type: journal.EventAgentLifecycle, Agent: &journal.AgentProvenance{
					Schema: "goobers.dev/journal/agent/v1", ID: id, RunID: "agent-run", Stage: stage, Attempt: logical,
					Lifecycle: lifecycle, StartedAt: started, UpdatedAt: updated, Usage: journal.AgentUsage{InputTokens: &cost},
				}}
				if err := (podArtifactRecorder{}).Append(event); err != nil {
					t.Fatal(err)
				}
			}
			// Keep another stage's lower physical ordinal and usage independent.
			emit("review", "1", 1, "reviewer", journal.AgentCompleted, 5, at, at)
			// A legacy event cannot displace an upgraded stage's stamped events.
			emit("work", "", 99, "copilot:work", journal.AgentCompleted, 999, at, at)
			emit("work", "2", previousLogical, "copilot:work", journal.AgentCompleted, 200, at, at)
			emit("work", "2", previousLogical, "old-child", journal.AgentWaiting, 0, at, at)
			emit("work", "3", 1, "copilot:work", journal.AgentWaiting, 30, at.Add(time.Minute), at.Add(time.Minute))
			// Old pods can keep emitting after the new visit has begun. Neither
			// the later arrival nor its later pod clock makes this current work.
			emit("work", "2", previousLogical, "copilot:work", journal.AgentFailed, 200, at, at.Add(10*time.Minute))
			active, err := reader.ActiveAgentTree("work", 1)
			if err != nil || len(active) != 1 || active["copilot:work"].Lifecycle != journal.AgentWaiting {
				t.Fatalf("current active visit=%v, err=%v", active, err)
			}
			assertPodAgentProjection(t, root, reader, journal.AgentWaiting, 5)
			emit("work", "3", 1, "copilot:work", journal.AgentCompleted, 30, at.Add(time.Minute), at.Add(2*time.Minute))
			emit("work", "2", previousLogical, "copilot:work", journal.AgentCompleted, 200, at, at.Add(20*time.Minute))
			// Preserve latest-attempt usage semantics: 30 + independent stage5,
			// not a new cumulative policy that adds all earlier attempts.
			assertPodAgentProjection(t, root, reader, journal.AgentCompleted, 35)
		})
	}
}

func assertPodAgentProjection(t *testing.T, root string, reader *journal.Reader, lifecycle journal.AgentLifecycle, cost int64) {
	t.Helper()
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := journal.AgentTree(events)
	if err != nil || len(tree) != 2 || tree["copilot:work"].Attempt != 1 || tree["copilot:work"].Lifecycle != lifecycle {
		t.Fatalf("aggregate current visit=%v, err=%v", tree, err)
	}
	receipt := stageCostReceipt(root, "agent-run")
	if receipt == nil || receipt.InputTokens == nil || *receipt.InputTokens != cost {
		t.Fatalf("provider receipt=%+v, want latest attempt cost %d", receipt, cost)
	}
	found := false
	for _, event := range events {
		key, _ := event.Runner[livejournal.EmitKeyRunnerField].(string)
		if strings.HasPrefix(key, "pod/3/work/agent.lifecycle/0/append/") {
			found = true
		}
	}
	if !found {
		t.Fatal("actual pod writer's physical identity was not persisted")
	}
}

func podAgentProjectionFixture(t *testing.T) (string, *journal.Reader) {
	t.Helper()
	root := t.TempDir()
	runsDir := instance.NewLayout(root).ForGaggle("test").RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: "agent-run", Gaggle: "test", Workflow: "workflow", WorkflowVersion: 1, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(g string) (string, bool) { return runsDir, g == "test" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		out, err := writer.Emit(r.Context(), req)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
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
	reader, err := journal.OpenRead(filepath.Join(runsDir, "agent-run"))
	if err != nil {
		t.Fatal(err)
	}
	return root, reader
}
