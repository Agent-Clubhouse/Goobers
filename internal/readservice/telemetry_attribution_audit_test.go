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
	"github.com/goobers/goobers/internal/telemetry"
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
	writeAuditRecordWithEnvironment(t, root, "before", "v1", true, "windows")
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
	if got := first.WorkflowFindings[0].Environments; len(got) != 1 || got[0] != "windows" {
		t.Fatalf("finding environments = %v, want stored span provenance", got)
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
	writeAuditRecordWithEnvironment(t, root, "healthy", "v2", false, "windows")
	reader.pages[0].Runs = append(reader.pages[0].Runs, terminalAuditRow("healthy", fixedAt.Add(time.Hour)))
	config.Now = fixedAt.Add(2 * time.Hour)
	recovered, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.WorkflowFindings) != 1 || recovered.WorkflowFindings[0].Verification != creditgraph.VerificationRecovered {
		t.Fatalf("recovered report = %+v, want recovered verification", recovered)
	}

	writeAuditRecordWithEnvironment(t, root, "repeated", "v1", true, "windows")
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

func TestStoredFaultAuditScopesCooldownToQuery(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecordForWorkflow(t, root, "first", "implementation", "v1", "shared worktree failure")
	writeAuditRecordForWorkflow(t, root, "second", "curation", "v1", "shared worktree failure")

	scopedReader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("first", now.Add(-time.Hour))},
	}}}
	scoped, err := StoredFaultAudit(
		context.Background(), root, scopedReader,
		StoredAttributionQuery{Workflow: "implementation"},
		creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.UnknownFindings) != 1 {
		t.Fatalf("scoped report = %+v, want one narrow mixed/unknown finding", scoped)
	}

	globalReader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{
			terminalAuditRow("first", now.Add(-time.Hour)),
			terminalAuditRow("second", now.Add(-30*time.Minute)),
		},
	}}}
	global, err := StoredFaultAudit(
		context.Background(), root, globalReader, StoredAttributionQuery{},
		creditgraph.FaultAuditConfig{Now: now.Add(time.Hour), SampleFloor: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	if global.Suppressed != 0 || len(global.ProductFindings) != 1 {
		t.Fatalf("global report = %+v, want unsuppressed cross-workflow product finding", global)
	}
}

func writeAuditRecord(t *testing.T, root, runID, version string, withCause bool) {
	t.Helper()
	writeAuditRecordWithEnvironment(t, root, runID, version, withCause, "")
}

func writeAuditRecordWithEnvironment(t *testing.T, root, runID, version string, withCause bool, environment string) {
	t.Helper()
	summary := ""
	if withCause {
		summary = "workflow instructions failed"
	}
	writeAuditRecordForWorkflowEnvironment(t, root, runID, "implementation", version, summary, environment)
}

func writeAuditRecordForWorkflow(t *testing.T, root, runID, workflow, version, summary string) {
	t.Helper()
	writeAuditRecordForWorkflowEnvironment(t, root, runID, workflow, version, summary, "")
}

func writeAuditRecordForWorkflowEnvironment(
	t *testing.T,
	root, runID, workflow, version, summary, environment string,
) {
	t.Helper()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        workflow,
		WorkflowVersion: 1,
		WorkflowDigest:  "sha256:workflow-" + version,
		GooberDigest:    "sha256:goober",
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if environment != "" {
		span, err := json.Marshal(telemetry.SpanRecord{
			Schema: telemetry.SpanSchema,
			Attributes: map[string]string{
				"deployment.environment": environment,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := run.RecordSpanWithSchema("implement", "runtime", telemetry.SpanSchema, span); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	runDir := run.Dir()
	record := creditgraph.RunRecord{
		Schema: creditgraph.RecordSchemaVersion, Status: creditgraph.RecordComplete,
		RunID: runID, Workflow: workflow, EffectiveVersion: version, Workload: "issue",
	}
	if summary != "" {
		record.Attribution = creditgraph.Attribution{
			Contributions: []creditgraph.Contribution{{
				NodeID: "stage", Stage: "implement", Path: []string{"stage"}, Confidence: 0.9,
			}},
			Causes: []creditgraph.CauseFinding{{
				NodeID: "stage", Stage: "implement", Class: creditgraph.ClassWeakInstructions,
				Confidence: 0.9, Summary: summary,
			}},
		}
		record.Evidence = []creditgraph.AttributionEvidenceLink{{
			RunID: runID, NodeID: "stage", Stage: "implement",
			Source: string(creditgraph.ClassWeakInstructions), JournalSequence: 1, JournalPath: "events.jsonl",
		}}
	} else {
		record.Attribution = creditgraph.Attribution{
			Contributions: []creditgraph.Contribution{{
				NodeID: "stage", Stage: "implement", Path: []string{"stage"}, Confidence: 0.9,
			}},
		}
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
	return readmodel.RunRow{
		RunID: runID, Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: finishedAt.Add(-time.Minute), FinishedAt: &finishedAt,
	}
}
