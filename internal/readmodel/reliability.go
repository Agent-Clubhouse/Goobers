package readmodel

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
)

// Stage-result conventions the reliability projection reads (#5313). A stage
// reports acceptance-criteria mapping evidence through scalar outputs, or by
// recording an artifact under AcceptanceMappingArtifact; a PR-opening stage
// reports whether the PR it opened is a draft alongside its prNumber.
const (
	AcceptanceMappingOutput       = "acceptanceMapping"
	AcceptanceMappingDigestOutput = "acceptanceMappingDigest"
	AcceptanceMappingArtifact     = "acceptance-mapping"
	PullRequestDraftOutput        = "draft"
	PullRequestNumberOutput       = "prNumber"

	// AcceptanceStateRecorded is reported when a mapping artifact is recorded
	// without a stage-reported state.
	AcceptanceStateRecorded = "recorded"
)

// Budget kinds a pinned workflow definition bounds (#5313). An agentic
// gate's repair branches are implementation review; a failure-class gate's
// fail branch is local validation repair; a ci-status gate's fail branch is
// provider remediation and its timeout branch CI polling. Each task's retry
// policy bounds its stage-policy retries, and the shared infrastructure
// allowance its local-infra retries, per stage pass.
const (
	BudgetImplementationReview = "implementation-review"
	BudgetStagePolicy          = "stage-policy"
	BudgetLocalInfra           = "local-infra"
	BudgetLocalValidation      = "local-validation"
	BudgetProviderRemediation  = "provider-remediation"
	BudgetCIPoll               = "ci-poll"
)

const (
	checkFailureClass = "failure-class"
	checkCIStatus     = "ci-status"
	outcomeFail       = "fail"
	outcomeInfra      = "infra"
	outcomeTimeout    = "timeout"
)

// ReliabilityAllowance is one budget the run's pinned definition bounds: the
// gate that charges it, the target stage it re-enters, and the allowance in
// effect when the run started.
type ReliabilityAllowance struct {
	Kind    string
	Gate    string
	Target  string
	Allowed int
}

// ReliabilityFacts are the journal facts the implementation reliability
// projection needs beyond the run summary's own fields. Both read paths fold
// them with After so list views agree with run detail.
type ReliabilityFacts struct {
	// TerminalCause is the durably recorded cause of the current terminal
	// generation; nil while running, after a resume, or for legacy journals.
	TerminalCause *journal.TerminalCause `json:",omitempty"`
	// PullRequestDraft is the draft state a stage reported for the PR it opened.
	PullRequestDraft *PullRequestDraft `json:",omitempty"`
	// AcceptanceState is the latest stage-reported acceptance mapping state.
	AcceptanceState string `json:",omitempty"`
	// AcceptanceDigest identifies the latest acceptance mapping artifact.
	AcceptanceDigest string `json:",omitempty"`
	// PolicyRepasses is the cumulative policy repass count per re-entered
	// target stage, as the gate journaled it (repassTarget/repassAttempt).
	PolicyRepasses map[string]int `json:",omitempty"`
	// TimeoutPolls is each gate's consecutive timeout-poll count; a
	// non-timeout outcome resets it.
	TimeoutPolls map[string]int `json:",omitempty"`
	// StageRetries counts each stage's policy and infrastructure retries in
	// its current pass; a fresh (non-retry) start resets them.
	StageRetries map[string]StageRetries `json:",omitempty"`
	// LastStage is the most recently started stage.
	LastStage string `json:",omitempty"`
	// Allowances are the budgets the run's trusted pinned definition bounds;
	// nil when no such definition is recorded. Not folded from events.
	Allowances []ReliabilityAllowance `json:",omitempty"`
}

// StageRetries are one stage pass's retry counts by class.
type StageRetries struct {
	Policy int `json:",omitempty"`
	Infra  int `json:",omitempty"`
}

// PullRequestDraft is a stage-reported draft state for one pull request.
type PullRequestDraft struct {
	ID    string
	Draft bool
}

// After folds one journal event into the facts.
func (f ReliabilityFacts) After(event journal.Event) ReliabilityFacts {
	if !event.KnownSchema() {
		return f
	}
	switch event.Type {
	case journal.EventRunFinished:
		f.TerminalCause = nil
		if event.TerminalCause != nil && event.TerminalCause.Schema == journal.TerminalCauseSchema {
			f.TerminalCause = cloneTerminalCause(*event.TerminalCause)
		}
	case journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested:
		f.TerminalCause = nil
	case journal.EventStageFinished:
		f = f.afterStageOutputs(event.Outputs)
	case journal.EventGateEvaluated:
		f = f.afterGateEvaluated(event)
	case journal.EventStageStarted:
		f = f.afterStageStarted(event)
	case journal.EventArtifactRecorded:
		if event.Name == AcceptanceMappingArtifact && event.Ref != nil && event.Ref.Digest != "" {
			f.AcceptanceDigest = event.Ref.Digest
			if f.AcceptanceState == "" {
				f.AcceptanceState = AcceptanceStateRecorded
			}
		}
	}
	return f
}

func (f ReliabilityFacts) afterStageOutputs(outputs map[string]any) ReliabilityFacts {
	if state, ok := outputs[AcceptanceMappingOutput].(string); ok && strings.TrimSpace(state) != "" {
		f.AcceptanceState = strings.ToLower(strings.TrimSpace(state))
		// A newly reported state supersedes any earlier artifact's digest
		// unless this result names its own.
		f.AcceptanceDigest = ""
	}
	if digest, ok := outputs[AcceptanceMappingDigestOutput].(string); ok && strings.TrimSpace(digest) != "" {
		f.AcceptanceDigest = strings.TrimSpace(digest)
		if f.AcceptanceState == "" {
			f.AcceptanceState = AcceptanceStateRecorded
		}
	}
	id, idOK := outputScalarString(outputs[PullRequestNumberOutput])
	draft, draftOK := outputBool(outputs[PullRequestDraftOutput])
	if idOK && draftOK {
		f.PullRequestDraft = &PullRequestDraft{ID: id, Draft: draft}
	}
	return f
}

// afterGateEvaluated mirrors runcontrol.RepassBudget from the journaled
// charge: human overrides and interrupted evaluations charge nothing.
func (f ReliabilityFacts) afterGateEvaluated(event journal.Event) ReliabilityFacts {
	if event.Actor != "" || event.Gate == "" {
		return f
	}
	if interrupted, _ := event.Runner["interrupted"].(bool); interrupted {
		return f
	}
	if event.Verdict == outcomeTimeout {
		if polls := runnerCount(event.Runner["pollAttempt"]); polls > 0 {
			f.TimeoutPolls = withCount(f.TimeoutPolls, event.Gate, polls)
		}
		return f
	}
	if _, polling := f.TimeoutPolls[event.Gate]; polling {
		f.TimeoutPolls = maps.Clone(f.TimeoutPolls)
		delete(f.TimeoutPolls, event.Gate)
	}
	target, _ := event.Runner["repassTarget"].(string)
	if attempt := event.RepassAttempt(); target != "" && event.Verdict != outcomeInfra && attempt > 0 {
		f.PolicyRepasses = withCount(f.PolicyRepasses, target, attempt)
	}
	return f
}

// afterStageStarted mirrors the runner's per-pass retry accounting (and
// runner.terminalPolicyRetries): policy and infrastructure retries advance
// their class, any other start opens a fresh pass.
func (f ReliabilityFacts) afterStageStarted(event journal.Event) ReliabilityFacts {
	if event.Stage == "" {
		return f
	}
	f.LastStage = event.Stage
	retries := StageRetries{}
	switch event.AttemptClass {
	case journal.AttemptPolicy:
		retries = f.StageRetries[event.Stage]
		retries.Policy++
	case journal.AttemptInfra:
		retries = f.StageRetries[event.Stage]
		retries.Infra++
	default:
		if _, seen := f.StageRetries[event.Stage]; !seen {
			return f
		}
	}
	f.StageRetries = maps.Clone(f.StageRetries)
	if f.StageRetries == nil {
		f.StageRetries = make(map[string]StageRetries)
	}
	if retries == (StageRetries{}) {
		delete(f.StageRetries, event.Stage)
	} else {
		f.StageRetries[event.Stage] = retries
	}
	return f
}

func withCount(counts map[string]int, key string, count int) map[string]int {
	counts = maps.Clone(counts)
	if counts == nil {
		counts = make(map[string]int)
	}
	counts[key] = count
	return counts
}

func runnerCount(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// PinnedReliabilityAllowances reads the budgets the run's trusted pinned
// definition bounds. It returns nil when the definition is missing or fails
// its integrity checks, so the projection reports those budgets as unknown
// rather than reading today's configuration.
func PinnedReliabilityAllowances(reader *journal.Reader, identity journal.RunIdentity) []ReliabilityAllowance {
	def, ok := trustedPinnedDefinition(reader, identity)
	if !ok {
		return nil
	}
	inherited := 0
	if identity.RunControls != nil {
		inherited = int(identity.RunControls.MaxRepasses)
	}
	return reliabilityAllowances(def.Spec, inherited)
}

func trustedPinnedDefinition(reader *journal.Reader, identity journal.RunIdentity) (workflow.Definition, bool) {
	for _, input := range identity.Inputs {
		if input.Name != journal.PinnedWorkflowDefinitionInputName {
			continue
		}
		if input.Integrity != apiv1.IntegrityTrusted {
			return workflow.Definition{}, false
		}
		data, err := reader.ArtifactBytes(input.Ref)
		if err != nil {
			return workflow.Definition{}, false
		}
		var def workflow.Definition
		if json.Unmarshal(data, &def) != nil || def.Name != identity.Workflow || def.Version != identity.WorkflowVersion {
			return workflow.Definition{}, false
		}
		digest, err := workflow.ComputeDigest(def)
		if err != nil || (identity.WorkflowDigest != "" && digest != identity.WorkflowDigest) {
			return workflow.Definition{}, false
		}
		return def, true
	}
	return workflow.Definition{}, false
}

// nonRepairOutcomes are the gate outcomes that never charge a policy repass.
var nonRepairOutcomes = map[string]bool{"pass": true, "approve": true, "escalate": true, outcomeInfra: true, outcomeTimeout: true}

func reliabilityAllowances(spec apiv1.WorkflowSpec, inherited int) []ReliabilityAllowance {
	var out []ReliabilityAllowance
	add := func(kind string, gate apiv1.Gate, outcome string, allowed int) {
		if target := gate.Branches[outcome]; target != "" && !strings.HasPrefix(target, "@") {
			out = append(out, ReliabilityAllowance{Kind: kind, Gate: gate.Name, Target: target, Allowed: allowed})
		}
	}
	for _, task := range spec.Tasks {
		policy := 0
		if task.Retry != nil {
			policy = max(0, int(task.Retry.MaxAttempts)-1)
		}
		out = append(out,
			ReliabilityAllowance{Kind: BudgetStagePolicy, Target: task.Name, Allowed: policy},
			ReliabilityAllowance{Kind: BudgetLocalInfra, Target: task.Name, Allowed: runcontrol.DefaultMaxInfrastructureAttempts - 1})
	}
	for _, gate := range spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic {
			for _, outcome := range slices.Sorted(maps.Keys(gate.Branches)) {
				if !nonRepairOutcomes[outcome] {
					add(BudgetImplementationReview, gate, outcome, runcontrol.MaxRepassesForGate(gate, inherited))
				}
			}
			continue
		}
		if gate.Evaluator != apiv1.EvaluatorAutomated || gate.Automated == nil {
			continue
		}
		switch gate.Automated.Check {
		case checkFailureClass:
			add(BudgetLocalValidation, gate, outcomeFail, runcontrol.MaxRepassesForGate(gate, inherited))
		case checkCIStatus:
			add(BudgetProviderRemediation, gate, outcomeFail, runcontrol.MaxRepassesForGate(gate, inherited))
			add(BudgetCIPoll, gate, outcomeTimeout, runcontrol.MaxTimeoutPollsForGate(gate))
		}
	}
	return out
}

func cloneTerminalCause(cause journal.TerminalCause) *journal.TerminalCause {
	for _, budget := range []**journal.TerminalBudget{&cause.Retry, &cause.Poll, &cause.Repass} {
		if *budget != nil {
			copy := **budget
			*budget = &copy
		}
	}
	return &cause
}

func outputScalarString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		return v, v != ""
	case float64:
		if v > 0 && v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
	case int:
		if v > 0 {
			return strconv.Itoa(v), true
		}
	}
	return "", false
}

func outputBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return parsed, err == nil
	}
	return false, false
}
