package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/featureusage"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/telemetry"
)

func usageHeartbeat(gaggle string, now time.Time) telemetry.DiagnosticRecord {
	return telemetry.DiagnosticRecord{Time: now, Name: fleetdiagnostics.HeartbeatEvent, Attributes: map[string]any{"schemaVersion": int64(1), "deploymentId": "deployment", "instanceId": "instance", "gaggleId": gaggle, "component": "daemon", "bootId": "boot", "bootStartedAt": now.Add(-time.Minute).Format(time.RFC3339Nano), "sequence": int64(1), "windowCoverage": "complete"}}
}
func TestFleetUsageConfiguredUnusedReloadAndRotatingCoverage(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	configured := map[string]map[string]bool{}
	heartbeats := []telemetry.DiagnosticRecord{}
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("g%d", i)
		configured[name] = map[string]bool{"adapter.codex": true}
		if err := os.MkdirAll(instance.NewLayout(root).ForGaggle(name).RunsDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		heartbeats = append(heartbeats, usageHeartbeat(name, now))
	}
	sampler := newFleetUsageSampler(root, func() map[string]map[string]bool { return configured })
	first := sampler(context.Background(), now, heartbeats)
	if len(first) != 9 {
		t.Fatal(len(first))
	}
	for _, record := range first {
		usage, err := fleetdiagnostics.DecodeFeatureUsage(record.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		if usage.GaggleID == "g8" {
			if usage.Count != nil || usage.Coverage == "complete" {
				t.Fatal("uninspected gaggle claimed zero", usage)
			}
		} else if usage.Count == nil || *usage.Count != 0 || !usage.Configured {
			t.Fatal(usage)
		}
	}
	configured["g8"]["adapter.codex"] = false
	second := sampler(context.Background(), now.Add(time.Second), heartbeats)
	if second[0].Attributes["gaggleId"] != "g8" || second[0].Attributes["configured"] != false || second[0].Attributes["count"] != int64(0) {
		t.Fatal("rotation/reload missing", second[0])
	}
	if records := newFleetUsageSampler(root, nil)(context.Background(), now, heartbeats); len(records) != 0 {
		t.Fatal("unknown configuration emitted false labels")
	}
}
func TestRemoteFeatureEvidenceCrossesRealJournalPlaneAndDeduplicates(t *testing.T) {
	runsDir := t.TempDir()
	now := time.Now().UTC()
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: "remote-run", Gaggle: "g", Workflow: "workflow", WorkflowVersion: 91, Driver: journal.DriverEngine}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: []byte(`{"dslVersion":"3.0"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runsDir, gaggle == "g" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	delivered := make(chan livejournal.EmitRequest, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		delivered <- req
		response, err := writer.Emit(r.Context(), req)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "remote-run")
	t.Setenv(dispatcher.EnvGaggle, "g")
	t.Setenv(dispatcher.EnvStage, "stage")
	t.Setenv(dispatcher.EnvPodToken, "synthetic-pod-token")
	t.Setenv(dispatcher.EnvPodAttempt, "attempt-1")
	recorder := podArtifactRecorder{dir: t.TempDir()}
	featureusage.RecordAdapter(recorder, "codex", "stage")
	featureusage.RecordAdapter(recorder, "codex", "stage")
	childAt := time.Now().UTC()
	if err := recorder.Append(journal.Event{Type: journal.EventAgentLifecycle, Stage: "stage", Agent: &journal.AgentProvenance{Schema: "goobers.dev/journal/agent/v1", ID: "child", ParentID: "parent", RunID: "remote-run", Stage: "stage", Attempt: 1, Lifecycle: journal.AgentStarted, StartedAt: childAt, UpdatedAt: childAt, Fidelity: journal.AgentFidelityPartial}}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Emit(context.Background(), lastUsageRequest(delivered)); err != nil {
		t.Fatal(err)
	}
	counts := featureusage.Scan(context.Background(), runsDir, now.Add(-time.Minute), time.Now().Add(time.Second))
	if counts["adapter.codex"].Value != 2 || counts["runner.engine"].Value != 1 || counts["dsl.v3"].Value != 1 || counts["capability.nested-agents"].Value != 1 || counts["capability.nested-agents"].Complete {
		t.Fatal(counts)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runsDir, "remote-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, event := range events {
		if event.Runner["kind"] == featureusage.Kind {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("remote retry produced %d observations", found)
	}
}

func lastUsageRequest(requests <-chan livejournal.EmitRequest) livejournal.EmitRequest {
	var last livejournal.EmitRequest
	for range 3 {
		last = <-requests
	}
	return last
}
