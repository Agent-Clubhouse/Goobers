package readservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

type pagedAttributionReader struct {
	readmodel.Reader
	pages []readmodel.ListPage
}

func (r *pagedAttributionReader) ListRuns(_ context.Context, options readmodel.ListOptions) (readmodel.ListPage, error) {
	if options.Cursor.Zero() {
		return r.pages[0], nil
	}
	return r.pages[1], nil
}

func TestStoredFaultAuditContinuesPastUnenrolledTerminalRows(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(filepath.Join(layout.RunsDir(), "unenrolled"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAuditRecord(t, root, "enrolled", "v1", true)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{
		{
			Runs:    []readmodel.RunRow{{RunID: "unenrolled", Terminal: true, StartedAt: now.Add(-time.Hour)}},
			HasMore: true,
			Next:    readmodel.ListCursor{Key: now.Add(-time.Hour), RunID: "unenrolled"},
		},
		{Runs: []readmodel.RunRow{{RunID: "enrolled", Terminal: true, StartedAt: now}}},
	}}

	report, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, creditgraph.FaultAuditConfig{
		Now: now, MaxObservations: 1, SampleFloor: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationsScanned != 1 || len(report.WorkflowFindings) != 1 {
		t.Fatalf("report = %+v, want later enrolled observation audited", report)
	}
}

func TestStoredFaultAuditPersistsCooldownAndPostFixVerification(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecord(t, root, "before", "v1", true)
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("before", now.Add(-time.Hour))},
	}}}
	config := creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1}

	first, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.WorkflowFindings) != 1 {
		t.Fatalf("first report = %+v, want workflow finding", first)
	}
	id := first.WorkflowFindings[0].ID

	config.Now = now.Add(time.Hour)
	second, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if second.Suppressed != 1 || len(second.WorkflowFindings) != 0 {
		t.Fatalf("second report = %+v, want durable cooldown suppression", second)
	}

	fixedAt := now.Add(2 * time.Hour)
	if err := RecordFaultAuditFix(context.Background(), root, id, fixedAt); err != nil {
		t.Fatal(err)
	}
	writeAuditRecord(t, root, "healthy", "v2", false)
	reader.pages[0].Runs = append(reader.pages[0].Runs, terminalAuditRow("healthy", fixedAt.Add(time.Hour)))
	config.Now = fixedAt.Add(2 * time.Hour)
	recovered, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.WorkflowFindings) != 1 || recovered.WorkflowFindings[0].Verification != creditgraph.VerificationRecovered {
		t.Fatalf("recovered report = %+v, want recovered verification", recovered)
	}

	writeAuditRecord(t, root, "repeated", "v1", true)
	reader.pages[0].Runs = append(reader.pages[0].Runs, terminalAuditRow("repeated", fixedAt.Add(90*time.Minute)))
	config.Now = fixedAt.Add(3 * time.Hour)
	repeated, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.WorkflowFindings) != 1 || repeated.WorkflowFindings[0].Verification != creditgraph.VerificationRepeated {
		t.Fatalf("repeated report = %+v, want repeated verification", repeated)
	}
}

func writeAuditRecord(t *testing.T, root, runID, version string, withCause bool) {
	t.Helper()
	runDir := filepath.Join(instance.NewLayout(root).RunsDir(), runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := creditgraph.RunRecord{
		Schema: creditgraph.RecordSchemaVersion, Status: creditgraph.RecordFailed,
		RunID: runID, Workflow: "implementation", EffectiveVersion: version,
	}
	if withCause {
		record.Attribution = creditgraph.Attribution{
			Contributions: []creditgraph.Contribution{{
				NodeID: "stage", Stage: "implement", Path: []string{"stage"}, Confidence: 0.9,
			}},
			Causes: []creditgraph.CauseFinding{{
				NodeID: "stage", Stage: "implement", Class: creditgraph.ClassWeakInstructions,
				Confidence: 0.9, Summary: "workflow instructions failed",
			}},
		}
		record.Evidence = []creditgraph.AttributionEvidenceLink{{
			RunID: runID, NodeID: "stage", Stage: "implement",
			Source: string(creditgraph.ClassWeakInstructions), JournalSequence: 1, JournalPath: "events.jsonl",
		}}
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(filepath.Join(runDir, creditgraph.RecordFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func terminalAuditRow(runID string, finishedAt time.Time) readmodel.RunRow {
	return readmodel.RunRow{RunID: runID, Terminal: true, StartedAt: finishedAt.Add(-time.Minute), FinishedAt: &finishedAt}
}
