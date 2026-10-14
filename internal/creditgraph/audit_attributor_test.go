package creditgraph

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// The tests in this file classify REAL attributor output: every observation is
// produced by journaling a failing run, publishing its attribution record with
// WriteRunRecord (Build → Attribute → evidence links), and reading the record
// back, exactly as the stored fault audit does. The attributor writes fixed
// summary text per failure class, and that text is full of component words
// ("harness or environment", "the model that ran …"), so hand-written
// summaries cannot exercise the precedence the auditor needs (#6086).

// failedStage describes the one stage of an attributed test run.
type failedStage struct {
	stage string
	// status is the stage.finished status: "failure" or "error" for a failing
	// run, "success" for a healthy one.
	status string
	// workflowDigest overrides the pinned workflow digest, to model a changed
	// workflow version.
	workflowDigest string
	// errorCode is the stage.finished normative error code, if any.
	errorCode string
	// requestedModel differs from the resolved model to produce a routing
	// cause; empty requests the resolved model.
	requestedModel string
	// artifact records stage evidence, so a stage with no tool calls is not
	// read as weak instructions.
	artifact bool
}

func attributedObservation(t *testing.T, runID, workflow string, failed failedStage, observedAt time.Time) AttributionObservation {
	t.Helper()
	definition, err := json.Marshal(map[string]any{
		"Name": workflow, "Version": 1, "dslVersion": "3.0",
		"Spec": map[string]any{"backprop": map[string]any{"enabled": true, "version": "v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := failed.workflowDigest
	if digest == "" {
		digest = "sha256:" + workflow
	}
	terminal, phase := "failed", journal.PhaseFailed
	if failed.status == "success" {
		terminal, phase = "completed", journal.PhaseCompleted
	}
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{
		RunID: runID, Workflow: workflow, WorkflowVersion: 1,
		WorkflowDigest: digest, GooberDigest: "sha256:goobers",
		Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "1"},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	appendStageEvents(t, run, runID, failed)
	runDir := filepath.Join(root, runID)
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: terminal}); err != nil {
		t.Fatalf("WriteRunRecord: %v", err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRunRecord: %v", err)
	}
	return AttributionObservation{
		RunID: record.RunID, Workflow: record.Workflow, WorkflowDigest: record.WorkflowDigest,
		EffectiveVersion: record.EffectiveVersion, Workload: record.Workload,
		Status: record.Status, Failure: record.Failure, RunPhase: phase,
		ObservedAt: observedAt, Attribution: record.Attribution,
		Evidence: append([]AttributionEvidenceLink(nil), record.Evidence...),
	}
}

func appendStageEvents(t *testing.T, run *journal.Run, runID string, failed failedStage) {
	t.Helper()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	tokens := int64(12)
	lifecycle := journal.AgentFailed
	if failed.status == "success" {
		lifecycle = journal.AgentCompleted
	}
	requested := failed.requestedModel
	if requested == "" {
		requested = "gpt-5"
	}
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: failed.stage, Attempt: 1},
		{Type: journal.EventAgentLifecycle, Stage: failed.stage, Attempt: 1, Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "root", RunID: runID, Stage: failed.stage, Attempt: 1,
			Lifecycle: lifecycle, RequestedModel: requested, ResolvedModel: "gpt-5",
			StartedAt: now, UpdatedAt: now, Usage: journal.AgentUsage{OutputTokens: &tokens},
		}},
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if failed.artifact {
		if _, err := run.RecordStageArtifact(failed.stage, 1, "", "diff", []byte("diff")); err != nil {
			t.Fatal(err)
		}
	}
	finished := journal.Event{Type: journal.EventStageFinished, Stage: failed.stage, Attempt: 1, Status: failed.status}
	if failed.errorCode != "" {
		finished.Error = &journal.ErrorDetail{Code: failed.errorCode, Message: "stage failed"}
	}
	if err := run.Append(finished); err != nil {
		t.Fatal(err)
	}
}

// attributedCohort journals one failing run per workflow, observations[i]
// in workflows[i], all failing the same way.
func attributedCohort(t *testing.T, prefix string, workflows []string, failed func(workflow string) failedStage) []AttributionObservation {
	t.Helper()
	observedAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	observations := make([]AttributionObservation, 0, len(workflows))
	for index, workflow := range workflows {
		runID := prefix + "-" + workflow + "-" + string(rune('a'+index))
		observations = append(observations, attributedObservation(t, runID, workflow, failed(workflow), observedAt))
	}
	return observations
}

// requireOnlyCause asserts each observation carries exactly the one class the
// scenario is about, with the attributor's template wording that used to
// misroute it.
func requireOnlyCause(t *testing.T, observations []AttributionObservation, class FailureClass, wording string) {
	t.Helper()
	for _, observation := range observations {
		if observation.Status != RecordComplete {
			t.Fatalf("record %s status = %q (%s), want complete", observation.RunID, observation.Status, observation.Failure)
		}
		causes := observation.Attribution.Causes
		if len(causes) != 1 || causes[0].Class != class || !strings.Contains(causes[0].Summary, wording) {
			t.Fatalf("run %s causes = %+v, want one %s cause whose summary says %q", observation.RunID, causes, class, wording)
		}
	}
}

func onlyFinding(t *testing.T, report FaultAuditReport, domain FaultDomain) FaultFinding {
	t.Helper()
	all := map[FaultDomain][]FaultFinding{
		FaultDomainProductRuntime: report.ProductFindings, FaultDomainExternal: report.ExternalFindings,
		FaultDomainWorkflow: report.WorkflowFindings, FaultDomainUnknown: report.UnknownFindings,
	}
	for candidate, findings := range all {
		want := 0
		if candidate == domain {
			want = 1
		}
		if len(findings) != want {
			t.Fatalf("report has %d %s findings, want %d: %+v", len(findings), candidate, want, report)
		}
	}
	return all[domain][0]
}

func TestAuditRealAttributorRuntimeFaultFollowsTheRecordedErrorCode(t *testing.T) {
	workflows := []string{"implementation", "review", "triage"}
	tests := []struct {
		name   string
		code   string
		domain FaultDomain
	}{
		{name: "shared worktree provisioning", code: telemetry.ErrCodeInfraWorkspace, domain: FaultDomainProductRuntime},
		{name: "claims lock contention", code: telemetry.ErrCodeClaimsLock, domain: FaultDomainProductRuntime},
		{name: "provider failure", code: telemetry.ErrCodeProviderFailed, domain: FaultDomainExternal},
		{name: "network failure", code: telemetry.ErrCodeInfraNet, domain: FaultDomainExternal},
		// No recorded code: the class rules the workflow out but names no
		// owner, so the finding stays unknown instead of defaulting to
		// external on the strength of the template's "harness" wording.
		{name: "no recorded code", domain: FaultDomainUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations := attributedCohort(t, "runtime", workflows, func(workflow string) failedStage {
				return failedStage{stage: workflow + "-stage", status: "error", errorCode: test.code, artifact: true}
			})
			requireOnlyCause(t, observations, ClassEnvironment, "harness or environment")
			finding := onlyFinding(t, AuditFaultDomains(observations, realAuditConfig()), test.domain)
			if len(finding.RunIDs) != 3 || len(finding.Workflows) != 3 {
				t.Fatalf("finding = %+v, want one cross-workflow signature over all three runs", finding)
			}
			if test.domain == FaultDomainUnknown && !containsString(finding.CounterEvidence, unownedEnvironmentEvidence) {
				t.Fatalf("counter-evidence = %q, want the unowned-environment explanation", finding.CounterEvidence)
			}
		})
	}
}

func TestAuditRealAttributorRoutingCauseIsAWorkflowDefect(t *testing.T) {
	observations := attributedCohort(t, "routing", []string{"implementation", "implementation", "implementation"},
		func(string) failedStage {
			return failedStage{stage: "implement", status: "failure", requestedModel: "claude-sonnet", artifact: true}
		})
	requireOnlyCause(t, observations, ClassRouting, "the model that ran")
	finding := onlyFinding(t, AuditFaultDomains(observations, realAuditConfig()), FaultDomainWorkflow)
	if len(finding.RunIDs) != 3 || finding.RecommendedOwner != "workflow/config owner" {
		t.Fatalf("finding = %+v, want a localized workflow-owned routing finding", finding)
	}
}

// TestAuditRealAttributorWorkflowFixRecoversOnTheAffectedPath pins that a
// post-fix run exercises the affected node path even though every attributed
// path runs through that run's own node.
func TestAuditRealAttributorWorkflowFixRecoversOnTheAffectedPath(t *testing.T) {
	routing := func(string) failedStage {
		return failedStage{stage: "implement", status: "failure", requestedModel: "claude-sonnet", artifact: true}
	}
	before := attributedCohort(t, "before", []string{"implementation", "implementation", "implementation"}, routing)
	config := realAuditConfig()
	finding := onlyFinding(t, AuditFaultDomains(before, config), FaultDomainWorkflow)

	fixedAt := config.Now
	healthy := make([]AttributionObservation, 0, 3)
	for _, runID := range []string{"after-a", "after-b", "after-c"} {
		healthy = append(healthy, attributedObservation(t, runID, "implementation", failedStage{
			stage: "implement", status: "success", workflowDigest: "sha256:implementation-fixed", artifact: true,
		}, fixedAt.Add(time.Hour)))
	}
	config.Now = fixedAt.Add(2 * time.Hour)
	config.FixesAppliedAt = map[string]time.Time{finding.ID: fixedAt}
	verified := onlyFinding(t, AuditFaultDomains(append(before, healthy...), config), FaultDomainWorkflow)
	if verified.Verification != VerificationRecovered {
		t.Fatalf("verification = %q, want recovered from three healthy post-fix runs of the fixed version", verified.Verification)
	}
}

func TestAuditRealAttributorWeakInstructionsStayPerWorkflowAndStage(t *testing.T) {
	workflows := []string{"implementation", "implementation", "implementation", "review", "review", "review"}
	observations := attributedCohort(t, "instructions", workflows, func(workflow string) failedStage {
		return failedStage{stage: workflow + "-stage", status: "failure"}
	})
	requireOnlyCause(t, observations, ClassWeakInstructions, "recorded no tool call")
	report := AuditFaultDomains(observations, realAuditConfig())
	if len(report.WorkflowFindings) != 2 || len(report.UnknownFindings)+len(report.ProductFindings)+len(report.ExternalFindings) != 0 {
		t.Fatalf("report = %+v, want two workflow-definition findings, one per workflow", report)
	}
	seen := map[string]bool{}
	for _, finding := range report.WorkflowFindings {
		if len(finding.Workflows) != 1 || len(finding.RunIDs) != 3 {
			t.Fatalf("finding = %+v, want three runs of one workflow", finding)
		}
		seen[finding.Workflows[0]] = true
	}
	if !seen["implementation"] || !seen["review"] {
		t.Fatalf("workflow findings = %+v, want one each for implementation and review", report.WorkflowFindings)
	}
}

// realAuditConfig is a pass shortly after the attributed runs were observed.
func realAuditConfig() FaultAuditConfig {
	return FaultAuditConfig{Now: time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC), SampleFloor: 3}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
