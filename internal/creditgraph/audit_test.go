package creditgraph

import (
	"reflect"
	"testing"
	"time"
)

func TestAuditFaultDomainsClassifiesSharedRuntimeOnce(t *testing.T) {
	var observations []AttributionObservation
	for i, workflow := range []string{"implementation", "review", "triage", "release"} {
		observations = append(observations, auditObservation(
			"run-"+workflow, workflow, "v"+string(rune('1'+i)), "runtime", "daemon worktree creation failed",
			ClassEnvironment, 0.9, "node:daemon",
		))
	}
	report := AuditFaultDomains(observations, FaultAuditConfig{SampleFloor: 3})
	if len(report.ProductFindings) != 1 || len(report.WorkflowFindings) != 0 {
		t.Fatalf("report = %+v, want one product finding", report)
	}
	finding := report.ProductFindings[0]
	if len(finding.RunIDs) != 4 || len(finding.Workflows) != 4 || len(finding.Evidence) != 4 {
		t.Fatalf("finding = %+v, want four affected runs/workflows with exact evidence", finding)
	}
	if finding.RecommendedOwner != "Goobers product reliability" || finding.Confidence < 0.8 {
		t.Fatalf("routing = %+v, want high-confidence product reliability", finding)
	}
}

func TestAuditFaultDomainsClassifiesLocalizedWorkflowVersionPath(t *testing.T) {
	observations := []AttributionObservation{
		auditObservation("run-1", "implementation", "version-a", "implement", "workflow selected an unsuitable tool", ClassBadToolChoice, 0.85, "stage:implement/tool:one"),
		auditObservation("run-2", "implementation", "version-a", "implement", "workflow selected an unsuitable tool", ClassBadToolChoice, 0.8, "stage:implement/tool:one"),
		auditObservation("run-3", "implementation", "version-a", "implement", "workflow selected an unsuitable tool", ClassBadToolChoice, 0.9, "stage:implement/tool:one"),
	}
	report := AuditFaultDomains(observations, FaultAuditConfig{})
	if len(report.WorkflowFindings) != 1 || report.WorkflowFindings[0].Domain != FaultDomainWorkflow {
		t.Fatalf("report = %+v, want one workflow finding", report)
	}
	if got := report.WorkflowFindings[0].NodePaths; !reflect.DeepEqual(got, [][]string{{"stage:implement", "tool:one"}}) {
		t.Fatalf("paths = %#v, want exact localized path", got)
	}
}

func TestAuditFaultDomainsPreservesSparseMixedAndMissingEvidence(t *testing.T) {
	sparse := auditObservation("sparse", "one", "v1", "stage", "daemon stopped", ClassEnvironment, 0.95, "daemon")
	missing := auditObservation("missing", "two", "", "stage", "daemon stopped", ClassEnvironment, 0.95, "daemon")
	missing.Evidence = nil
	contradictory := auditObservation("contradictory", "three", "v3", "stage", "daemon stopped", ClassEnvironment, 0.95, "daemon")
	contradictory.Attribution.Causes[0].Assumptions = []string{"contradictory successful daemon evidence exists"}
	report := AuditFaultDomains([]AttributionObservation{sparse, missing, contradictory}, FaultAuditConfig{SampleFloor: 4})
	if len(report.UnknownFindings) != 1 {
		t.Fatalf("report = %+v, want one unknown finding", report)
	}
	finding := report.UnknownFindings[0]
	if finding.Confidence > 0.3 || len(finding.CounterEvidence) < 3 {
		t.Fatalf("finding = %+v, want reduced confidence and explicit counter-evidence", finding)
	}
}

func TestAuditFaultDomainsDeduplicatesRunsPassesAndBoundsOutput(t *testing.T) {
	one := auditObservation("duplicate", "one", "v1", "stage", "weak workflow instructions", ClassWeakInstructions, 0.8, "stage")
	two := auditObservation("second", "one", "v1", "stage", "weak workflow instructions", ClassWeakInstructions, 0.8, "stage")
	three := auditObservation("third", "one", "v1", "stage", "weak workflow instructions", ClassWeakInstructions, 0.8, "stage")
	config := FaultAuditConfig{MaxFindings: 1, MaxRunsPerFinding: 2, MaxEvidence: 1}
	first := AuditFaultDomains([]AttributionObservation{one, one, two, three}, config)
	second := AuditFaultDomains([]AttributionObservation{three, two, one}, config)
	if first.WorkflowFindings[0].ID != second.WorkflowFindings[0].ID {
		t.Fatalf("finding IDs changed across passes: %q != %q", first.WorkflowFindings[0].ID, second.WorkflowFindings[0].ID)
	}
	if len(first.WorkflowFindings[0].RunIDs) != 2 || len(first.WorkflowFindings[0].Evidence) != 1 {
		t.Fatalf("finding was not bounded: %+v", first.WorkflowFindings[0])
	}
}

func TestAuditFaultDomainsCooldownAndPostFixVerification(t *testing.T) {
	before := auditObservation("before", "one", "v1", "stage", "weak workflow instructions", ClassWeakInstructions, 0.8, "stage")
	report := AuditFaultDomains([]AttributionObservation{before}, FaultAuditConfig{SampleFloor: 1})
	id := report.WorkflowFindings[0].ID
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	before.ObservedAt = now.Add(-2 * time.Hour)
	healthy := AttributionObservation{RunID: "after", Workflow: "one", EffectiveVersion: "v2", ObservedAt: now}
	verified := AuditFaultDomains([]AttributionObservation{before, healthy}, FaultAuditConfig{
		Now: now, SampleFloor: 1, FixesAppliedAt: map[string]time.Time{id: now.Add(-time.Hour)},
	})
	if verified.WorkflowFindings[0].Verification != VerificationRecovered {
		t.Fatalf("verification = %q, want recovered", verified.WorkflowFindings[0].Verification)
	}
	suppressed := AuditFaultDomains([]AttributionObservation{before}, FaultAuditConfig{
		Now: now, SampleFloor: 1, PreviousReports: map[string]time.Time{id: now.Add(-time.Hour)},
	})
	if suppressed.Suppressed != 1 || len(suppressed.WorkflowFindings) != 0 {
		t.Fatalf("cooldown report = %+v, want one suppressed finding", suppressed)
	}
}

func auditObservation(runID, workflow, version, stage, summary string, class FailureClass, confidence float64, node string) AttributionObservation {
	path := []string{node}
	if class == ClassBadToolChoice {
		path = []string{"stage:implement", "tool:one"}
	}
	return AttributionObservation{
		RunID: runID, Workflow: workflow, EffectiveVersion: version, Workload: "issue",
		Status: RecordComplete, Environments: []string{"windows"},
		Attribution: Attribution{
			Contributions: []Contribution{{NodeID: node, Stage: stage, Path: path, Confidence: confidence}},
			Causes:        []CauseFinding{{NodeID: node, Stage: stage, Class: class, Confidence: confidence, Summary: summary}},
		},
		Evidence: []AttributionEvidenceLink{{
			RunID: runID, NodeID: node, Stage: stage, Source: string(class), Detail: summary,
			JournalSequence: 7, JournalPath: "gaggles/core/runs/" + runID + "/events.jsonl",
			ArtifactPath: "artifacts/evidence.json", ArtifactDigest: "sha256:evidence",
		}},
	}
}
