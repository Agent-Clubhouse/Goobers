package httpapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
)

var fixedTelemetryTime = time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)

// TestImplementationOutcomeWireShapesAgree pins the three restatements of one
// shape against each other: the rollup row the daemon reads, the read
// service's projection that goes on the wire, and the stage-side client's
// restatement of that wire shape (internal/telemetryclient restates rather
// than imports, exactly as internal/claimsclient does — so this is the test
// that keeps the restatement honest).
//
// Compared by MARSHALLED JSON rather than by field list, because the wire is
// what actually has to agree: a renamed tag, a dropped omitempty, or a
// reordered field would all show up here.
func TestImplementationOutcomeWireShapesAgree(t *testing.T) {
	row := rollup.ImplementationOutcome{
		RunID:        "run-3",
		ItemID:       "912",
		Status:       "failed",
		StartedAt:    fixedTelemetryTime,
		FinishedAt:   fixedTelemetryTime,
		Stage:        "implement",
		ErrorCode:    "harness.crash",
		ErrorMessage: "boom",
		Gate:         "review",
		Verdict:      "reject",
	}
	projected := readservice.TelemetryImplementationOutcome{
		RunID:        row.RunID,
		ItemID:       row.ItemID,
		Status:       row.Status,
		StartedAt:    row.StartedAt,
		FinishedAt:   row.FinishedAt,
		Stage:        row.Stage,
		ErrorCode:    row.ErrorCode,
		ErrorMessage: row.ErrorMessage,
		Gate:         row.Gate,
		Verdict:      row.Verdict,
	}
	restated := telemetryclient.ImplementationOutcome{
		RunID:        row.RunID,
		ItemID:       row.ItemID,
		Status:       row.Status,
		StartedAt:    row.StartedAt,
		FinishedAt:   row.FinishedAt,
		Stage:        row.Stage,
		ErrorCode:    row.ErrorCode,
		ErrorMessage: row.ErrorMessage,
		Gate:         row.Gate,
		Verdict:      row.Verdict,
	}

	rowJSON := mustMarshal(t, row)
	projectedJSON := mustMarshal(t, projected)
	restatedJSON := mustMarshal(t, restated)
	if projectedJSON != rowJSON {
		t.Fatalf("read-service projection = %s, rollup row = %s", projectedJSON, rowJSON)
	}
	if restatedJSON != projectedJSON {
		t.Fatalf("client restatement = %s, read-service projection = %s", restatedJSON, projectedJSON)
	}

	// A client decode of a server encode must round-trip every field, so the
	// stage sees the evidence the daemon sent rather than a lossy subset.
	var decoded telemetryclient.ImplementationOutcome
	if err := json.Unmarshal([]byte(projectedJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, restated) {
		t.Fatalf("decoded = %+v, want %+v", decoded, restated)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
