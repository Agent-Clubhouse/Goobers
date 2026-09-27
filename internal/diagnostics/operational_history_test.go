package diagnostics

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/history"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestOperationalEvidenceReadsBoundedSnapshotAndDeliveryHistoryOffline(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join((instance.Layout{Root: root}).SchedulerDir(), "diagnostics")
	store, err := history.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attrs := map[string]any{"schemaVersion": 1, "deploymentId": "d", "instanceId": "i", "component": "daemon", "bootId": "b", "bootStartedAt": now.Format(time.RFC3339Nano), "sequence": 1, "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Format(time.RFC3339Nano), "windowCoverage": "complete", "state": "idle", "reasonCode": "no_eligible_work", "diagnosticsDroppedRecords": 0, "ownerRef": "private-owner-marker"}
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{{Time: now, Name: "goobers.fleet.heartbeat", Attributes: attrs}}); err != nil {
		t.Fatal(err)
	}
	attrs["sequence"], attrs["diagnosticsDroppedRecords"], attrs["observedAt"] = 2, 2, now.Add(time.Second).Format(time.RFC3339Nano)
	if err := store.Append(context.Background(), []telemetry.DiagnosticRecord{{Time: now.Add(time.Second), Name: "goobers.fleet.heartbeat", Attributes: attrs}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	evidence := collectOperationalEvidence(root)
	if evidence.LocalHistory == nil || len(evidence.Observations) != 1 || len(evidence.Delivery) != 2 || evidence.Delivery[1].DroppedRecords != 2 {
		t.Fatalf("missing public historical evidence: %+v", evidence)
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-owner-marker") {
		t.Fatal("offline preview exposed routing identity")
	}
	if len(evidence.Gaps) == 0 || evidence.Coverage != "retained-window" {
		t.Fatal("snapshot claimed uninterrupted coverage", evidence)
	}
}
