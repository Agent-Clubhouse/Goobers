package journalequivalence

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// §9.4 allows clock/run/host/duration metadata, not arbitrary payload keys
// with those names. These paths follow journal.Event and readmodel.Projection;
// '*' traverses only a schema-declared array, never an arbitrary object.
// There are no host/duration fields in the current corpus' event envelopes.
// New metadata locations require explicit review; unknown locations compare.
var eventVolatilePaths = []string{"time", "runId"}
var projectionVolatilePaths = []string{
	"Run.RunID", "Run.StartedAt", "Run.FinishedAt", "Run.LastActivity",
	"Run.Operator.LastHeartbeatAt", "Run.Operator.LatestErrorAt", "Run.Operator.ReviewAt",
	"Run.Operator.Activity.Active.*.startedAt",
	"Run.Operator.Activity.Active.*.executionObservedAt", "Run.Operator.Activity.Active.*.executionDeadline",
	"Run.Operator.RetryBackoff.waits.*.observedAt", "Run.Operator.RetryBackoff.waits.*.deadline",
	"Stages.*.RunID", "Stages.*.StartedAt", "Stages.*.FinishedAt",
	"Nodes.*.RunID", "NodeParents.*.RunID", "Remediation.*.RunID", "Remediation.*.ObservedAt",
}

func maskCanonicalMetadata(decoded, source any, runID string) {
	var paths []string
	switch source.(type) {
	case journal.Event:
		paths = eventVolatilePaths
		root, _ := decoded.(map[string]any)
		ref, _ := root["externalRef"].(map[string]any)
		// The managed branch identity contains this run's ID. Keep its provider,
		// kind, branch prefix and all other external-reference text exact.
		if ref["kind"] == "branch" {
			normalizeRunIDPath(decoded, "externalRef.id", runID)
		}
	case readmodel.Projection:
		paths = projectionVolatilePaths
		normalizeRunIDPath(decoded, "Run.Operator.WorkspaceBranch", runID)
	default:
		return // Untyped/business payloads have no envelope metadata authority.
	}
	for _, path := range paths {
		transformMetadataPath(decoded, strings.Split(path, "."), func(value any) any {
			if value == nil || value == "" || value == "0001-01-01T00:00:00Z" {
				return value
			}
			return "<masked>"
		})
	}
}

func normalizeRunIDPath(value any, path, runID string) {
	transformMetadataPath(value, strings.Split(path, "."), func(value any) any {
		text, ok := value.(string)
		if !ok || runID == "" {
			return value
		}
		return strings.ReplaceAll(text, runID, "<run-id>")
	})
}

func transformMetadataPath(value any, path []string, transform func(any) any) {
	if len(path) == 0 {
		return
	}
	if path[0] == "*" {
		if list, ok := value.([]any); ok {
			for _, entry := range list {
				transformMetadataPath(entry, path[1:], transform)
			}
		}
		return
	}
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	next, ok := object[path[0]]
	if !ok {
		return
	}
	if len(path) == 1 {
		object[path[0]] = transform(next)
		return
	}
	transformMetadataPath(next, path[1:], transform)
}

func TestCanonicalizerMasksOnlyExactMetadataPaths(t *testing.T) {
	const id = "11111111111111111111111111111111"
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	event := journal.Event{Time: now, RunID: id, Name: "artifact-" + id, Outputs: map[string]any{"host": "business-host", "time": "business-time", "runId": id, "durationMs": 42}}
	var decoded map[string]any
	if err := json.Unmarshal(canonicalJSON(t, event, id), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["time"] != "<masked>" || decoded["runId"] != "<masked>" || decoded["name"] != "artifact-"+id {
		t.Fatalf("metadata=%v", decoded)
	}
	payload := decoded["outputs"].(map[string]any)
	if payload["host"] != "business-host" || payload["time"] != "business-time" || payload["runId"] != id || payload["durationMs"] != float64(42) {
		t.Fatalf("payload masked: %v", payload)
	}
	var projection readmodel.Projection
	projection.Run.StartedAt = now
	unset := canonicalJSON(t, projection, id)
	zeroTime := time.Time{}
	projection.Run.FinishedAt = &zeroTime
	zero := canonicalJSON(t, projection, id)
	projection.Run.FinishedAt = &now
	set := canonicalJSON(t, projection, id)
	if bytes.Equal(unset, set) || bytes.Equal(zero, set) || bytes.Equal(unset, zero) {
		t.Fatalf("optional timestamp presence collapsed: null=%s zero=%s set=%s", unset, zero, set)
	}
}

func TestCanonicalizerPreservesBusinessVolatilityNames(t *testing.T) {
	const id = "11111111111111111111111111111111"
	for _, key := range []string{"host", "time", "runId", "durationMs", "startedAt", "FinishedAt"} {
		t.Run(key, func(t *testing.T) {
			event := func(value string) journal.Event {
				return journal.Event{Outputs: map[string]any{"customerRecord": map[string]any{key: value}, "items": []any{map[string]any{key: value}}}}
			}
			if bytes.Equal(canonicalJSON(t, event("business-a"), id), canonicalJSON(t, event("business-b"), id)) {
				t.Fatalf("semantic outputs.%s was masked", key)
			}
		})
	}
	// Even an exact-looking metadata path embedded in business data is normative.
	payload := map[string]any{"Run": map[string]any{"StartedAt": "business-a"}}
	before, _ := json.Marshal(payload)
	if got := canonicalJSON(t, payload, id); !bytes.Equal(got, before) {
		t.Fatalf("untyped business object changed: %s", got)
	}
}
