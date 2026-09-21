package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestOperationalBundleOfflineProjectionAndRedaction(t *testing.T) {
	root := t.TempDir()
	log, _, err := journal.OpenInstanceLog((instance.Layout{Root: root}).SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	err = log.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "goobers.fleet.heartbeat", "diagnostic": map[string]any{
		"schemaVersion": 1, "instanceId": "instance-one", "gaggleId": "gaggle-one", "component": "daemon", "bootId": "boot-one", "version": "v0.5.0", "buildCommit": "commit-one", "platform": "linux/amd64",
		"observedAt": "2026-09-20T00:00:00Z", "windowStart": "2026-09-19T23:00:00Z", "windowCoverage": "partial", "lastUsefulProgressAt": "2026-09-19T23:30:00Z", "state": "stalled", "reasonCode": "no_progress",
		"requiredMcpState": "active", "requiredMcpCoverage": "partial", "requiredMcpReason": "tool_authorization_failure", "requiredMcpObservedAt": "2026-09-19T23:50:00Z", "requiredMcpActiveCount": 1, "requiredMcpAdapter": "copilot-cli", "requiredMcpStage": "implement",
		"backlogState": "attention", "backlogReasonCode": "pending_without_confirmed_progress", "backlogCoverage": "complete", "backlogPendingCount": 2, "backlogObservedAt": "2026-09-19T23:59:45Z",
		"prompt": "private-prompt-marker", "code": "private-source-marker", "ownerRef": "private-owner-marker", "rawError": "private-error-marker",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	} // Daemon is down before collection.
	collector := collectorForTest()
	bundle, err := collector.Collect(Options{Root: root, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Operational == nil || len(bundle.Operational.Observations) != 1 {
		t.Fatalf("missing offline evidence: %+v", bundle.Operational)
	}
	observation := bundle.Operational.Observations[0]
	if observation.Version != "v0.5.0" || observation.ReasonCode != "no_progress" || observation.LastUsefulProgressAt == "" || observation.BootID != "boot-one" {
		t.Fatalf("missing diagnostic context: %+v", observation)
	}
	if observation.RequiredMCP == nil || observation.RequiredMCP.Reason != "tool_authorization_failure" || observation.RequiredMCP.Stage != "implement" {
		t.Fatal("offline MCP context missing", observation)
	}
	if observation.Backlog == nil || observation.Backlog.PendingCount == nil || *observation.Backlog.PendingCount != 2 || observation.Backlog.State != "attention" {
		t.Fatal("offline pending-work evidence missing", observation)
	}
	if !strings.Contains(Summary(bundle), "claim availability unknown") {
		t.Fatal("pending work summary overstates eligibility")
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(data) + Summary(bundle)
	for _, private := range []string{"private-prompt-marker", "private-source-marker", "private-owner-marker", "private-error-marker"} {
		if strings.Contains(rendered, private) {
			t.Fatalf("bundle exported %s", private)
		}
	}
	if !strings.Contains(rendered, "historical") {
		t.Fatal("missing observation limitation")
	}
}
func TestOperationalBundleMissingHistoryIsExplicit(t *testing.T) {
	evidence := collectOperationalEvidence(t.TempDir())
	if evidence.Coverage != "unavailable" || len(evidence.Gaps) == 0 || len(evidence.Observations) != 0 {
		t.Fatalf("missing history claimed complete: %+v", evidence)
	}
}
