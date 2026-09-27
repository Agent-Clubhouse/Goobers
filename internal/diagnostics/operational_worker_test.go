package diagnostics

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperationalWorkerProjection(t *testing.T) {
	attrs := map[string]any{"observedAt": "2026-09-20T00:00:00Z", "workerObservedAt": "2026-09-20T00:00:00Z", "workerObservation": "no_recent_poller", "workerCoverage": "engine_workflow_activity_queue", "missingWorkerCount": float64(1), "rawError": "private-worker-error", "ownerRef": "private-contact"}
	observation := projectOperationalObservation(attrs)
	if observation.Worker == nil || observation.Worker.MissingCount == nil || *observation.Worker.MissingCount != 1 {
		t.Fatal(observation)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-") {
		t.Fatal(string(data))
	}
	attrs["workerUnexpected"] = "private-worker-identity"
	if got := projectOperationalObservation(attrs); got.Worker != nil {
		t.Fatal("unknown payload retained", got)
	}
}
