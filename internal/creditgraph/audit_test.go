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

func TestAuditFaultDomainsAggregatesConflictingClassesByStableSignature(t *testing.T) {
	workflow := auditObservation("workflow", "implementation", "v1", "implement", "operation failed", ClassWeakInstructions, 0.9, "stage")
	external := auditObservation("external", "review", "v2", "review", "operation failed", ClassEnvironment, 0.9, "stage")
	product := auditObservation("product", "release", "v3", "release", "operation failed", ClassEnvironment, 0.9, "stage")
	product.Attribution.Causes[0].Evidence = []string{"scheduler operation failed"}

	report := AuditFaultDomains([]AttributionObservation{workflow, external, product}, FaultAuditConfig{SampleFloor: 3})
	if len(report.UnknownFindings) != 1 {
		t.Fatalf("report = %+v, want one mixed finding for the stable signature", report)
	}
	if finding := report.UnknownFindings[0]; finding.Signature != "operation failed" || finding.Confidence > 0.45 {
		t.Fatalf("finding = %+v, want class-independent signature with reduced confidence", finding)
	}
}

func TestAuditFaultDomainsCapsUnknownOnlyConfidence(t *testing.T) {
	observations := []AttributionObservation{
		auditObservation("one", "implementation", "v1", "stage", "unclassified failure", ClassUnknown, 0.95, "node"),
		auditObservation("two", "implementation", "v1", "stage", "unclassified failure", ClassUnknown, 0.95, "node"),
		auditObservation("three", "implementation", "v1", "stage", "unclassified failure", ClassUnknown, 0.95, "node"),
	}
	report := AuditFaultDomains(observations, FaultAuditConfig{SampleFloor: 3})
	if len(report.UnknownFindings) != 1 || report.UnknownFindings[0].Confidence > 0.45 {
		t.Fatalf("report = %+v, want reduced-confidence unknown finding", report)
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
	healthy := auditObservation("after", "one", "v2", "stage", "", ClassUnknown, 0.8, "stage")
	healthy.ObservedAt = now
	healthy.Attribution.Causes = nil
	healthy.Evidence = nil
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

func TestAuditFaultDomainsPostFixVerificationRequiresMatchingCohort(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fixedAt := now.Add(-time.Hour)
	before := auditObservation("before", "one", "v1", "stage", "weak workflow instructions", ClassWeakInstructions, 0.8, "stage")
	before.ObservedAt = now.Add(-2 * time.Hour)
	initial := AuditFaultDomains([]AttributionObservation{before}, FaultAuditConfig{Now: now, SampleFloor: 1})
	id := initial.WorkflowFindings[0].ID

	matching := auditObservation("after", "one", "v2", "stage", "", ClassUnknown, 0.8, "stage")
	matching.ObservedAt = now
	matching.Attribution.Causes = nil
	matching.Evidence = nil
	tests := []struct {
		name   string
		mutate func(*AttributionObservation)
	}{
		{name: "workflow", mutate: func(observation *AttributionObservation) { observation.Workflow = "other" }},
		{name: "workload", mutate: func(observation *AttributionObservation) { observation.Workload = "schedule" }},
		{name: "effective version", mutate: func(observation *AttributionObservation) { observation.EffectiveVersion = "v1" }},
		{name: "node path", mutate: func(observation *AttributionObservation) {
			observation.Attribution.Contributions[0].Path = []string{"other"}
		}},
		{name: "environment", mutate: func(observation *AttributionObservation) { observation.Environments = []string{"linux"} }},
		{name: "failure signature", mutate: func(observation *AttributionObservation) {
			observation.Attribution.Causes = []CauseFinding{{
				NodeID: "stage", Stage: "stage", Class: ClassWeakInstructions,
				Confidence: 0.8, Summary: "unrelated workflow failure",
			}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unrelated := matching
			unrelated.Attribution.Contributions = append([]Contribution(nil), matching.Attribution.Contributions...)
			unrelated.Environments = append([]string(nil), matching.Environments...)
			test.mutate(&unrelated)
			report := AuditFaultDomains([]AttributionObservation{before, unrelated}, FaultAuditConfig{
				Now: now, SampleFloor: 1, FixesAppliedAt: map[string]time.Time{id: fixedAt},
			})
			if got := report.WorkflowFindings[0].Verification; got != VerificationPending {
				t.Fatalf("verification = %q, want pending for unrelated %s cohort", got, test.name)
			}
		})
	}

	report := AuditFaultDomains([]AttributionObservation{before, matching}, FaultAuditConfig{
		Now: now, SampleFloor: 1, FixesAppliedAt: map[string]time.Time{id: fixedAt},
	})
	if got := report.WorkflowFindings[0].Verification; got != VerificationRecovered {
		t.Fatalf("verification = %q, want recovered for matching held-out cohort", got)
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
