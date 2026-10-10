package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestStoredFaultAuditDegradesFailedRecordWhenJournalIsMissing(t *testing.T) {
	root := t.TempDir()
	runID := "missing-journal"
	writeAuditRecord(t, root, runID, "v1", true)
	runDir := filepath.Join(instance.NewLayout(root).RunsDir(), runID)
	recordPath := filepath.Join(runDir, creditgraph.RecordFileName)
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record creditgraph.RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Status = creditgraph.RecordFailed
	record.Failure = "journal provenance unavailable"
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(recordPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == creditgraph.RecordFileName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(runDir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow(runID, now)},
	}}}
	report, err := StoredFaultAudit(
		context.Background(), root, reader, StoredAttributionQuery{},
		creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationsScanned != 1 || len(report.UnknownFindings) != 1 {
		t.Fatalf("report = %+v, want one degraded unknown finding", report)
	}
	if confidence := report.UnknownFindings[0].Confidence; confidence > 0.3 {
		t.Fatalf("confidence = %v, want reduced confidence for missing journal provenance", confidence)
	}
}

func TestStoredFaultAuditRejectsMalformedPersistedBaseline(t *testing.T) {
	root := t.TempDir()
	path := faultAuditStatePath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{
		"schema":"goobers.dev/backprop/fault-audit-state/v1",
		"baselineObservations":{"backprop-00000000000000000000":[{"runId":""}]}
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := StoredFaultAudit(
		context.Background(), root, &pagedAttributionReader{pages: []readmodel.ListPage{{}}},
		StoredAttributionQuery{}, creditgraph.FaultAuditConfig{},
	)
	if err == nil || !strings.Contains(err.Error(), "decode fault audit state: baseline finding") {
		t.Fatalf("error = %v, want explicit malformed baseline error", err)
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

func TestStoredFaultAuditPersistsFindingAfterBaselineLeavesWindow(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecordWithEnvironment(t, root, "before", "v1", true, "windows")
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("before", now.Add(-time.Hour))},
	}}}
	config := creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1}
	initial, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.WorkflowFindings) != 1 {
		t.Fatalf("initial report = %+v, want workflow finding", initial)
	}
	id := initial.WorkflowFindings[0].ID
	fixedAt := now.Add(time.Hour)
	if err := RecordFaultAuditFix(context.Background(), root, id, fixedAt); err != nil {
		t.Fatal(err)
	}

	writeAuditRecordWithEnvironment(t, root, "healthy", "v2", false, "windows")
	healthyAt := fixedAt.Add(time.Hour)
	healthyRow := terminalAuditRow("healthy", healthyAt)
	healthyRow.Phase = journal.PhaseFailed
	reader.pages[0].Runs = []readmodel.RunRow{healthyRow}
	config.Now = healthyAt.Add(time.Hour)
	pending, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{
		Since: fixedAt,
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.WorkflowFindings) != 1 ||
		pending.WorkflowFindings[0].ID != id ||
		pending.WorkflowFindings[0].Verification != creditgraph.VerificationPending {
		t.Fatalf("pending report = %+v, want durable pending finding", pending)
	}

	reader.pages[0].Runs = []readmodel.RunRow{terminalAuditRow("healthy", healthyAt)}
	config.Now = healthyAt.Add(2 * time.Hour)
	recovered, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{
		Since: fixedAt,
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.WorkflowFindings) != 1 ||
		recovered.WorkflowFindings[0].ID != id ||
		recovered.WorkflowFindings[0].Verification != creditgraph.VerificationRecovered {
		t.Fatalf("recovered report = %+v, want durable recovered finding", recovered)
	}

	writeAuditRecordWithEnvironment(t, root, "repeated", "v2", true, "windows")
	repeatedAt := healthyAt.Add(time.Hour)
	reader.pages[0].Runs = []readmodel.RunRow{terminalAuditRow("repeated", repeatedAt)}
	config.Now = repeatedAt.Add(time.Hour)
	repeated, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{
		Since: fixedAt,
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.WorkflowFindings) != 1 ||
		repeated.WorkflowFindings[0].ID != id ||
		repeated.WorkflowFindings[0].Verification != creditgraph.VerificationRepeated {
		t.Fatalf("repeated report = %+v, want durable repeated finding", repeated)
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

func TestShadowBackpropRecordNeverReachesFaultAuditFiling(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecordWithEnvironment(t, root, "shadow", "v1", true, "windows")
	runDir := filepath.Join(instance.NewLayout(root).RunsDir(), "shadow")
	data, err := os.ReadFile(filepath.Join(runDir, creditgraph.RecordFileName))
	if err != nil {
		t.Fatal(err)
	}
	var record creditgraph.RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Mode = "shadow"
	if data, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(filepath.Join(runDir, creditgraph.ShadowRecordFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runDir, creditgraph.RecordFileName)); err != nil {
		t.Fatal(err)
	}
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("shadow", now.Add(-time.Hour))},
	}}}
	config := creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1}

	preview, err := PreviewStoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	filed, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	for name, report := range map[string]creditgraph.FaultAuditReport{"preview": preview, "filing": filed} {
		findings := len(report.ProductFindings) + len(report.ExternalFindings) + len(report.WorkflowFindings) + len(report.UnknownFindings)
		if report.ObservationsScanned != 0 || findings != 0 || report.Suppressed != 0 {
			t.Fatalf("%s report = %+v, want shadow attribution invisible", name, report)
		}
	}
	state, err := os.ReadFile(faultAuditStatePath(root))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "backprop-") {
		t.Fatalf("fault audit state = %s, want no cooldown or baseline from shadow attribution", state)
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

func TestPreviewStoredFaultAuditDoesNotConsumeFilingCooldown(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecordWithEnvironment(t, root, "before", "v1", true, "windows")
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("before", now.Add(-time.Hour))},
	}}}
	config := creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1}

	for pass := range 2 {
		preview, err := PreviewStoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
		if err != nil {
			t.Fatal(err)
		}
		if preview.Suppressed != 0 || len(preview.WorkflowFindings) != 1 {
			t.Fatalf("preview pass %d = %+v, want the finding shown every time", pass, preview)
		}
	}
	filed, err := StoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if filed.Suppressed != 0 || len(filed.WorkflowFindings) != 1 {
		t.Fatalf("filing pass = %+v, want previews not to start its cooldown", filed)
	}
}

func TestPreviewStoredFaultAuditFindingCanBeMarkedFixed(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	writeAuditRecordWithEnvironment(t, root, "before", "v1", true, "windows")
	reader := &pagedAttributionReader{pages: []readmodel.ListPage{{
		Runs: []readmodel.RunRow{terminalAuditRow("before", now.Add(-time.Hour))},
	}}}
	preview, err := PreviewStoredFaultAudit(context.Background(), root, reader, StoredAttributionQuery{},
		creditgraph.FaultAuditConfig{Now: now, SampleFloor: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.WorkflowFindings) != 1 {
		t.Fatalf("preview = %+v, want one workflow finding", preview)
	}
	if err := RecordFaultAuditFix(context.Background(), root, preview.WorkflowFindings[0].ID, now); err != nil {
		t.Fatalf("mark fix for a previewed finding: %v", err)
	}
}

func TestFaultAuditStatePrunesStaleUnfixedEntries(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-faultAuditStateRetention - time.Hour)
	fixedID := "backprop-00000000000000000001"
	staleID := "backprop-00000000000000000002"
	freshID := "backprop-00000000000000000003"
	observation := func(at time.Time) []creditgraph.AttributionObservation {
		return []creditgraph.AttributionObservation{{RunID: "run", ObservedAt: at}}
	}
	state := faultAuditState{
		PreviousReports: map[string]time.Time{fixedID: stale, staleID: stale, freshID: now},
		FixesAppliedAt:  map[string]time.Time{fixedID: stale},
		BaselineObservations: map[string][]creditgraph.AttributionObservation{
			fixedID: observation(stale), "scope-ab:" + staleID: observation(stale), freshID: observation(now),
		},
	}
	pruneFaultAuditState(&state, now)
	if _, ok := state.PreviousReports[staleID]; ok {
		t.Fatalf("stale unfixed cooldown retained: %+v", state.PreviousReports)
	}
	if _, ok := state.BaselineObservations["scope-ab:"+staleID]; ok {
		t.Fatalf("stale unfixed baseline retained: %+v", state.BaselineObservations)
	}
	for _, id := range []string{fixedID, freshID} {
		if _, ok := state.PreviousReports[id]; !ok {
			t.Fatalf("report %s pruned, want fixed and fresh entries kept", id)
		}
		if _, ok := state.BaselineObservations[id]; !ok {
			t.Fatalf("baseline %s pruned, want fixed and fresh entries kept", id)
		}
	}
}
