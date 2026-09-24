package creditgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestWriteRunRecordRequiresExplicitEnrollment(t *testing.T) {
	for _, test := range []struct {
		name     string
		backprop any
		want     bool
	}{
		{name: "omitted"},
		{name: "disabled", backprop: map[string]any{"enabled": false, "version": "v1"}},
		{name: "enabled", backprop: map[string]any{"enabled": true, "version": "v1"}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, runDir := recordTestRun(t, test.backprop)
			defer func() { _ = run.Close() }()
			if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "act", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "act", Attempt: 1, Status: "success"}); err != nil {
				t.Fatal(err)
			}
			wrote, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "completed"})
			if err != nil {
				t.Fatalf("WriteRunRecord: %v", err)
			}
			if wrote != test.want {
				t.Fatalf("wrote = %v, want %v", wrote, test.want)
			}
			_, statErr := os.Stat(filepath.Join(runDir, RecordFileName))
			if test.want && statErr != nil {
				t.Fatalf("record missing: %v", statErr)
			}
			if !test.want && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unenrolled run record stat = %v, want not exist", statErr)
			}
		})
	}
}

func TestRunRecordPinsIdentityAndExactEvidence(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1"})
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "act", Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "act", Attempt: 1, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatalf("WriteRunRecord: %v", err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRunRecord: %v", err)
	}
	if record.ContractVersion != "v1" || record.RunID != "run-enabled" ||
		record.Workflow != "implementation" || record.WorkflowVersion != 7 ||
		record.WorkflowDigest != "sha256:workflow" || record.EffectiveVersion == "" ||
		record.Workload != string(journal.TriggerItem) {
		t.Fatalf("record identity = %+v", record)
	}
	if record.Status != RecordComplete || len(record.Attribution.Contributions) == 0 {
		t.Fatalf("record attribution = %+v", record)
	}
	var exact bool
	for _, link := range record.Evidence {
		if link.NodeID == "stage:act#1" && link.JournalPath == "events.jsonl" && link.JournalSequence > 0 {
			exact = true
		}
	}
	if !exact {
		t.Fatalf("record evidence = %+v, want exact stage journal sequence", record.Evidence)
	}
}

func TestRunRecordPinsCompleteEffectiveVersion(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1"})
	defer func() { _ = run.Close() }()
	transcript := []byte(`{"role":"assistant","model":"gpt-5.6-sol"}`)
	if _, err := run.RecordSpanWithSchema("act", "transcript", telemetry.GenAIEventSchema, transcript); err != nil {
		t.Fatal(err)
	}
	span, err := json.Marshal(telemetry.SpanRecord{
		Schema: telemetry.SpanSchema,
		Attributes: map[string]string{
			telemetry.AttrHarnessVersion: "copilot-cli/1.0.86",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpanWithSchema("act", "task/act", telemetry.SpanSchema, span); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	want := (rollup.EffectiveVersion{
		WorkflowDigest: "sha256:workflow",
		GooberDigest:   "sha256:goobers",
		Model:          "gpt-5.6-sol",
		HarnessVersion: "copilot-cli/1.0.86",
	}).Hash()
	if record.EffectiveVersion != want {
		t.Fatalf("effective version = %q, want %q", record.EffectiveVersion, want)
	}
}

func TestRunRecordPersistsExactGateToolAndRuntimeEvidence(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1"})
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "act", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	transcript := []byte(strings.Join([]string{
		`{"role":"assistant","model":"gpt-5.6-sol","tool_call":{"id":"call-1","name":"bash"}}`,
		`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
	}, "\n"))
	toolRef, err := run.RecordSpanWithSchema("act", "transcript", telemetry.GenAIEventSchema, transcript)
	if err != nil {
		t.Fatal(err)
	}
	runtimeSpan, err := json.Marshal(telemetry.SpanRecord{
		Schema: telemetry.SpanSchema,
		Attributes: map[string]string{
			telemetry.AttrHarnessVersion: "copilot-cli/1.0.86",
			"deployment.environment":     "production",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeRef, err := run.RecordSpanWithSchema("act", "runtime", telemetry.SpanSchema, runtimeSpan)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail",
		Stage: "act", Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "act", Attempt: 1, Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	found := map[NodeKind]bool{}
	for _, link := range record.Evidence {
		contribution, ok := record.Attribution.Contribution(link.NodeID)
		if !ok {
			continue
		}
		switch contribution.Kind {
		case KindEvaluator:
			found[KindEvaluator] = link.JournalSequence > 0 && link.ArtifactDigest == ""
		case KindTool:
			found[KindTool] = link.JournalSequence > 0 && link.ArtifactDigest == toolRef.Digest
		case KindRuntime:
			found[KindRuntime] = link.JournalSequence > 0 && link.ArtifactDigest == runtimeRef.Digest
		case KindEnvironment:
			found[KindEnvironment] = link.JournalSequence > 0 && link.ArtifactDigest == runtimeRef.Digest
		}
	}
	for _, kind := range []NodeKind{KindEvaluator, KindTool, KindRuntime, KindEnvironment} {
		if !found[kind] {
			t.Fatalf("missing exact %s evidence: %+v", kind, record.Evidence)
		}
	}
}

func TestRunRecordDoesNotCohortMixedEffectiveVersions(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1"})
	defer func() { _ = run.Close() }()
	for i, model := range []string{"gpt-5.6-sol", "claude-opus-5"} {
		transcript := []byte(fmt.Sprintf(`{"role":"assistant","model":%q}`, model))
		if _, err := run.RecordSpanWithSchema("act", "transcript", telemetry.GenAIEventSchema, transcript); err != nil {
			t.Fatalf("record span %d: %v", i, err)
		}
	}
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if record.EffectiveVersion != "" {
		t.Fatalf("mixed effective version = %q, want uncohortable", record.EffectiveVersion)
	}
}

func TestRunRecordMarksMissingProvenanceInsufficient(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1"})
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "act", Attempt: 1, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatalf("WriteRunRecord: %v", err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != RecordInsufficientEvidence {
		t.Fatalf("status = %q, want %q", record.Status, RecordInsufficientEvidence)
	}
}

func TestRunRecordSurfacesCorruptSpanAsAnalysisFailure(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1"})
	defer func() { _ = run.Close() }()
	ref, err := run.RecordSpanWithSchema("act", "task/act", telemetry.SpanSchema, []byte(`{"schema":"goobers.dev/telemetry/span/v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, filepath.FromSlash(ref.Path)), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	wrote, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "completed"})
	if !wrote {
		t.Fatal("WriteRunRecord did not persist the enrolled run's failure")
	}
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("WriteRunRecord error = %v, want span digest mismatch", err)
	}
	record, readErr := ReadRunRecord(runDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if record.Status != RecordFailed || !strings.Contains(record.Failure, "digest mismatch") {
		t.Fatalf("record failure = (%q, %q), want persisted digest mismatch", record.Status, record.Failure)
	}
	if record.EffectiveVersion != "" {
		t.Fatalf("failed record effective version = %q, want uncohortable", record.EffectiveVersion)
	}
	if got := AggregateAttributionEvidence([]AttributionObservation{{
		RunID:            record.RunID,
		Workflow:         record.Workflow,
		EffectiveVersion: record.EffectiveVersion,
		Workload:         record.Workload,
		Status:           record.Status,
		Failure:          record.Failure,
		Attribution:      record.Attribution,
		Evidence:         record.Evidence,
	}}); len(got) != 0 {
		t.Fatalf("failed record cohorts = %+v, want none", got)
	}
}

func recordTestRun(t *testing.T, backprop any) (*journal.Run, string) {
	t.Helper()
	spec := map[string]any{}
	if backprop != nil {
		spec["backprop"] = backprop
	}
	definition, err := json.Marshal(map[string]any{
		"Name": "implementation", "Version": 7, "dslVersion": "3.0", "Spec": spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{
		RunID: "run-enabled", Workflow: "implementation", WorkflowVersion: 7,
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goobers",
		Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "5459"},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	return run, filepath.Join(root, "run-enabled")
}
