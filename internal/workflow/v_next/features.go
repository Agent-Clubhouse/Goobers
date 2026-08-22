package vnext

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// FeatureID is the stable, author-facing name of one DSL capability.
type FeatureID string

// SupportLevel describes the compatibility promise for a DSL feature.
type SupportLevel string

const (
	// SupportPreview marks an unstable feature that requires explicit acknowledgement.
	SupportPreview SupportLevel = "preview"
	// SupportGA marks a stable, generally available feature.
	SupportGA SupportLevel = "ga"
	// SupportDeprecated marks a supported feature scheduled for removal.
	SupportDeprecated SupportLevel = "deprecated"
	// SupportRemoved marks a feature that validation must reject.
	SupportRemoved SupportLevel = "removed"
)

// SupportTransition records when a DSL feature entered one support level.
type SupportTransition struct {
	Level        SupportLevel `json:"level"`
	SinceVersion string       `json:"sinceVersion"`
}

// Feature records a DSL feature's current support level and complete lifecycle.
type Feature struct {
	ID                    FeatureID           `json:"id"`
	Level                 SupportLevel        `json:"level"`
	SinceVersion          string              `json:"sinceVersion"`
	Replacement           FeatureID           `json:"replacement,omitempty"`
	RemovalTargetVersion  string              `json:"removalTargetVersion,omitempty"`
	LastSupportingVersion string              `json:"lastSupportingVersion,omitempty"`
	DSLVersions           []DSLFeatureSupport `json:"dslVersions,omitempty"`
	History               []SupportTransition `json:"history"`
}

// DSLFeatureSupport records a feature's support level in one DSL version.
// Absence means that version does not contain the feature.
type DSLFeatureSupport struct {
	Version string       `json:"version"`
	Level   SupportLevel `json:"level"`
}

// FeatureRegistry is an immutable lookup table of DSL feature support.
type FeatureRegistry struct {
	entries map[FeatureID]Feature
}

// NewFeatureRegistry validates and copies entries into a feature registry.
func NewFeatureRegistry(features []Feature) (FeatureRegistry, error) {
	entries := make(map[FeatureID]Feature, len(features))
	for _, feature := range features {
		if strings.TrimSpace(string(feature.ID)) == "" {
			return FeatureRegistry{}, fmt.Errorf("DSL feature ID must not be empty")
		}
		if !validSupportLevel(feature.Level) {
			return FeatureRegistry{}, fmt.Errorf("DSL feature %q has unknown support level %q", feature.ID, feature.Level)
		}
		if strings.TrimSpace(feature.SinceVersion) == "" {
			return FeatureRegistry{}, fmt.Errorf("DSL feature %q has an empty since-version", feature.ID)
		}
		seenVersions := make(map[string]struct{}, len(feature.DSLVersions))
		for _, version := range feature.DSLVersions {
			if _, ok := supportmatrix.GetDSL().Lookup(version.Version); !ok {
				return FeatureRegistry{}, fmt.Errorf("DSL feature %q references unknown DSL version %q", feature.ID, version.Version)
			}
			switch version.Level {
			case SupportPreview, SupportGA, SupportDeprecated, SupportRemoved:
			default:
				return FeatureRegistry{}, fmt.Errorf("DSL feature %q has unknown support level %q for DSL version %q", feature.ID, version.Level, version.Version)
			}
			if _, exists := seenVersions[version.Version]; exists {
				return FeatureRegistry{}, fmt.Errorf("DSL feature %q has duplicate DSL version %q", feature.ID, version.Version)
			}
			seenVersions[version.Version] = struct{}{}
		}
		if err := validateFeatureHistory(feature); err != nil {
			return FeatureRegistry{}, fmt.Errorf("DSL feature %q: %w", feature.ID, err)
		}
		switch feature.Level {
		case SupportDeprecated:
			if strings.TrimSpace(string(feature.Replacement)) == "" {
				return FeatureRegistry{}, fmt.Errorf("deprecated DSL feature %q has no replacement", feature.ID)
			}
			if strings.TrimSpace(feature.RemovalTargetVersion) == "" {
				return FeatureRegistry{}, fmt.Errorf("deprecated DSL feature %q has no removal-target version", feature.ID)
			}
		case SupportRemoved:
			if strings.TrimSpace(feature.LastSupportingVersion) == "" {
				return FeatureRegistry{}, fmt.Errorf("removed DSL feature %q has no last-supporting version", feature.ID)
			}
		}
		if _, exists := entries[feature.ID]; exists {
			return FeatureRegistry{}, fmt.Errorf("duplicate DSL feature %q", feature.ID)
		}
		entries[feature.ID] = cloneFeature(feature)
	}
	return FeatureRegistry{entries: entries}, nil
}

// Lookup returns the support metadata for id.
func (r FeatureRegistry) Lookup(id FeatureID) (Feature, bool) {
	feature, ok := r.entries[id]
	return cloneFeature(feature), ok
}

// All returns every feature in stable ID order.
func (r FeatureRegistry) All() []Feature {
	features := make([]Feature, 0, len(r.entries))
	for _, feature := range r.entries {
		features = append(features, cloneFeature(feature))
	}
	sort.Slice(features, func(i, j int) bool {
		return features[i].ID < features[j].ID
	})
	return features
}

func cloneFeature(feature Feature) Feature {
	feature.History = slices.Clone(feature.History)
	feature.DSLVersions = slices.Clone(feature.DSLVersions)
	return feature
}

func newFeatureRegistryAgainstReleased(released FeatureRegistry, features []Feature) (FeatureRegistry, error) {
	current, err := NewFeatureRegistry(features)
	if err != nil {
		return FeatureRegistry{}, err
	}
	if err := validateFeatureRegistryEvolution(released, current); err != nil {
		return FeatureRegistry{}, err
	}
	return current, nil
}

func validateFeatureRegistryEvolution(released, current FeatureRegistry) error {
	for _, previous := range released.All() {
		candidate, ok := current.Lookup(previous.ID)
		if !ok {
			return fmt.Errorf("released DSL feature %q must remain in the registry", previous.ID)
		}
		if len(candidate.History) < len(previous.History) ||
			!slices.Equal(candidate.History[:len(previous.History)], previous.History) {
			return fmt.Errorf("released DSL feature %q lifecycle history must not change", previous.ID)
		}
	}

	for _, candidate := range current.All() {
		if candidate.Level != SupportRemoved {
			continue
		}
		previous, ok := released.Lookup(candidate.ID)
		if !ok || (previous.Level != SupportDeprecated && previous.Level != SupportRemoved) {
			return fmt.Errorf(
				"DSL feature %q must be deprecated in the latest released registry before removal",
				candidate.ID,
			)
		}
	}

	// Per-version availability is append-only (#3292): History pins the
	// feature-level lifecycle above, but without this a released feature could
	// silently drop a DSL version from DSLVersions — vanishing from
	// FeaturesAtDSLVersion for workflows pinned to that version — or regress a
	// version's level (say ga back to preview) with no record. Every version
	// entry the released registry declared must remain declared, and its level
	// may only advance along the same lifecycle transitions the registry level
	// follows. The rule is scoped to the versions this registry declares at
	// all: released snapshots come from the merged cross-interpreter
	// workflow.AllFeatures(), so they carry versions (1.4) another interpreter
	// serves — whole-version retirement is the support matrix's policy, while
	// one feature losing a version its siblings keep is exactly the accident
	// caught here. This loop runs after the removal-policy loop so an invalid
	// removal is still reported as a removal-policy violation.
	currentVersions := make(map[string]struct{})
	for _, candidate := range current.All() {
		for _, support := range candidate.DSLVersions {
			currentVersions[support.Version] = struct{}{}
		}
	}
	for _, previous := range released.All() {
		candidate, _ := current.Lookup(previous.ID)
		for _, releasedSupport := range previous.DSLVersions {
			if _, declared := currentVersions[releasedSupport.Version]; !declared {
				continue
			}
			currentLevel, ok := dslVersionLevel(candidate, releasedSupport.Version)
			if !ok {
				return fmt.Errorf(
					"released DSL feature %q must remain available at DSL version %q",
					previous.ID,
					releasedSupport.Version,
				)
			}
			if currentLevel != releasedSupport.Level &&
				!validSupportTransition(releasedSupport.Level, currentLevel) {
				return fmt.Errorf(
					"released DSL feature %q at DSL version %q may not move from %q to %q",
					previous.ID,
					releasedSupport.Version,
					releasedSupport.Level,
					currentLevel,
				)
			}
		}
	}
	return nil
}

func dslVersionLevel(feature Feature, version string) (SupportLevel, bool) {
	for _, support := range feature.DSLVersions {
		if support.Version == version {
			return support.Level, true
		}
	}
	return "", false
}

func validateFeatureHistory(feature Feature) error {
	if len(feature.History) == 0 {
		return fmt.Errorf("lifecycle history must not be empty")
	}

	for i, transition := range feature.History {
		if !validSupportLevel(transition.Level) {
			return fmt.Errorf("lifecycle history has unknown support level %q", transition.Level)
		}
		if i == 0 {
			if transition.Level != SupportPreview && transition.Level != SupportGA {
				return fmt.Errorf("lifecycle must start at preview or ga, not %q", transition.Level)
			}
			if _, err := parseReleaseVersion(transition.SinceVersion, true); err != nil {
				return fmt.Errorf("invalid initial version %q: %w", transition.SinceVersion, err)
			}
			continue
		}

		previous := feature.History[i-1]
		if !validSupportTransition(previous.Level, transition.Level) {
			return fmt.Errorf("invalid lifecycle transition %q -> %q", previous.Level, transition.Level)
		}
		previousVersion, err := parseReleaseVersion(previous.SinceVersion, true)
		if err != nil {
			return fmt.Errorf("invalid version %q: %w", previous.SinceVersion, err)
		}
		currentVersion, err := parseReleaseVersion(transition.SinceVersion, false)
		if err != nil {
			return fmt.Errorf("invalid version %q: %w", transition.SinceVersion, err)
		}
		if previous.SinceVersion != initialFeatureSinceVersion &&
			compareReleaseVersions(previousVersion, currentVersion) >= 0 {
			return fmt.Errorf(
				"lifecycle version %q must follow %q",
				transition.SinceVersion,
				previous.SinceVersion,
			)
		}
		if transition.Level == SupportRemoved &&
			!isLaterMinor(previousVersion, currentVersion) {
			return fmt.Errorf(
				"feature deprecated in %q must remain deprecated until a later minor release before removal in %q",
				previous.SinceVersion,
				transition.SinceVersion,
			)
		}
	}

	current := feature.History[len(feature.History)-1]
	if feature.Level != current.Level || feature.SinceVersion != current.SinceVersion {
		return fmt.Errorf(
			"current support %q since %q does not match lifecycle history %q since %q",
			feature.Level,
			feature.SinceVersion,
			current.Level,
			current.SinceVersion,
		)
	}
	return nil
}

func validSupportLevel(level SupportLevel) bool {
	switch level {
	case SupportPreview, SupportGA, SupportDeprecated, SupportRemoved:
		return true
	default:
		return false
	}
}

func validSupportTransition(from, to SupportLevel) bool {
	switch from {
	case SupportPreview:
		return to == SupportGA || to == SupportDeprecated
	case SupportGA:
		return to == SupportDeprecated
	case SupportDeprecated:
		return to == SupportRemoved
	default:
		return false
	}
}

type releaseVersion struct {
	major uint64
	minor uint64
	patch uint64
}

func parseReleaseVersion(value string, allowDevelopment bool) (releaseVersion, error) {
	if value == initialFeatureSinceVersion {
		if allowDevelopment {
			return releaseVersion{}, nil
		}
		return releaseVersion{}, fmt.Errorf("%q is only valid for the initial pre-release baseline", value)
	}
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, "v") {
		return releaseVersion{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return releaseVersion{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
	}
	numbers := make([]uint64, len(parts))
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return releaseVersion{}, fmt.Errorf("must use canonical vMAJOR.MINOR.PATCH")
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return releaseVersion{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
		}
		numbers[i] = number
	}
	return releaseVersion{major: numbers[0], minor: numbers[1], patch: numbers[2]}, nil
}

func compareReleaseVersions(left, right releaseVersion) int {
	switch {
	case left.major != right.major:
		return cmpUint64(left.major, right.major)
	case left.minor != right.minor:
		return cmpUint64(left.minor, right.minor)
	default:
		return cmpUint64(left.patch, right.patch)
	}
}

func cmpUint64(left, right uint64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func isLaterMinor(deprecated, removed releaseVersion) bool {
	return removed.major > deprecated.major ||
		(removed.major == deprecated.major && removed.minor > deprecated.minor)
}

func (r FeatureRegistry) resolve(ids []FeatureID) ([]Feature, error) {
	features := make([]Feature, 0, len(ids))
	var missing []string
	for _, id := range ids {
		feature, ok := r.Lookup(id)
		if !ok {
			missing = append(missing, string(id))
			continue
		}
		features = append(features, feature)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("DSL feature registry is missing: %s", strings.Join(missing, ", "))
	}
	return features, nil
}

// LookupFeature returns support metadata from the current DSL registry.
func LookupFeature(id FeatureID) (Feature, bool) {
	return currentFeatureRegistry.Lookup(id)
}

// AllFeatures returns a stable snapshot of the current DSL registry.
func AllFeatures() []Feature {
	return currentFeatureRegistry.All()
}

// FeaturesAtDSLVersion filters a feature snapshot to one DSL version.
func FeaturesAtDSLVersion(features []Feature, version string) ([]Feature, error) {
	filtered := make([]Feature, 0, len(features))
	for _, feature := range features {
		for _, versionSupport := range feature.DSLVersions {
			if versionSupport.Version != version {
				continue
			}
			projected := cloneFeature(feature)
			projected.Level = versionSupport.Level
			filtered = append(filtered, projected)
			break
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].ID < filtered[j].ID
	})
	return filtered, nil
}

const (
	featureWorkflowGaggle                 FeatureID = "workflow.spec.gaggle"
	featureWorkflowDisplayName            FeatureID = "workflow.spec.displayName"
	featureWorkflowTriggers               FeatureID = "workflow.spec.triggers"
	featureWorkflowReadiness              FeatureID = "workflow.spec.readiness"
	featureWorkflowRunControls            FeatureID = "workflow.spec.runControls"
	featureWorkflowMaxConcurrentRuns      FeatureID = "workflow.spec.readiness.maxConcurrentRuns"
	featureWorkflowMaxRunsPerHour         FeatureID = "workflow.spec.readiness.maxRunsPerHour"
	featureWorkflowMaxRunsPerDay          FeatureID = "workflow.spec.readiness.maxRunsPerDay"
	featureWorkflowMaxChainDepth          FeatureID = "workflow.spec.readiness.maxChainDepth"
	featureWorkflowMaxOpenPRs             FeatureID = "workflow.spec.readiness.maxOpenPRs"
	featureWorkflowStart                  FeatureID = "workflow.spec.start"
	featureWorkflowTasks                  FeatureID = "workflow.spec.tasks"
	featureWorkflowGates                  FeatureID = "workflow.spec.gates"
	featureWorkflowParallels              FeatureID = "workflow.spec.parallels"
	featureParallelFailurePolicy          FeatureID = "workflow.spec.parallels.failurePolicy"
	featureParallelBranches               FeatureID = "workflow.spec.parallels.branches"
	featureParallelJoin                   FeatureID = "workflow.spec.parallels.join"
	featureParallelOnFailure              FeatureID = "workflow.spec.parallels.onFailure"
	featureParallelBranchTimeout          FeatureID = "workflow.spec.parallels.branchTimeoutSeconds"
	featureParallelMaxConcurrentBranches  FeatureID = "workflow.spec.parallels.maxConcurrentBranches"
	featureWorkflowTerminalComplete       FeatureID = "workflow.terminal.complete"
	featureWorkflowTerminalAbort          FeatureID = "workflow.terminal.abort"
	featureWorkflowTerminalEscalate       FeatureID = "workflow.terminal.escalate"
	featureGaggleSandbox                  FeatureID = "gaggle.spec.sandbox"
	featureGaggleCheckoutSparse           FeatureID = "gaggle.spec.project.checkout.sparse"
	featureGooberGaggle                   FeatureID = "goober.spec.gaggle"
	featureGooberRole                     FeatureID = "goober.spec.role"
	featureGooberDisplayName              FeatureID = "goober.spec.displayName"
	featureGooberInstructions             FeatureID = "goober.spec.instructions"
	featureGooberHarnessCopilot           FeatureID = "goober.spec.harness.copilot"
	featureGooberHarnessClaudeCode        FeatureID = "goober.spec.harness.claude-code"
	featureGooberModel                    FeatureID = "goober.spec.model"
	featureGooberHarnessOptions           FeatureID = "goober.spec.harnessOptions"
	featureGooberTimeoutSeconds           FeatureID = "goober.spec.timeoutSeconds"
	featureGooberCapabilities             FeatureID = "goober.spec.capabilities"
	featureGooberSkills                   FeatureID = "goober.spec.skills"
	featureGooberTools                    FeatureID = "goober.spec.tools"
	featureGooberMCPServers               FeatureID = "goober.spec.mcpServers"
	featureGooberScaleFactor              FeatureID = "goober.spec.scaleFactor"
	featureGooberWorkflows                FeatureID = "goober.spec.workflows"
	featureTriggerManual                  FeatureID = "trigger.manual"
	featureTriggerBacklogItem             FeatureID = "trigger.backlog-item"
	featureTriggerBacklogItemSelector     FeatureID = "trigger.backlog-item.selector"
	featureTriggerBacklogItemTrustLabel   FeatureID = "trigger.backlog-item.trustLabel"
	featureTriggerLabelPredicate          FeatureID = "trigger.labelPredicate"
	featureTriggerFieldPredicate          FeatureID = "trigger.fieldPredicate"
	featureTriggerSchedule                FeatureID = "trigger.schedule"
	featureTriggerSignal                  FeatureID = "trigger.signal"
	featureTriggerWebhook                 FeatureID = "trigger.webhook"
	featureTaskName                       FeatureID = "task.name"
	featureTaskDeterministic              FeatureID = "task.deterministic"
	featureTaskAgentic                    FeatureID = "task.agentic"
	featureTaskGoal                       FeatureID = "task.goal"
	featureTaskGoober                     FeatureID = "task.goober"
	featureTaskInputs                     FeatureID = "task.inputs"
	featureTaskInputFieldOrder            FeatureID = "task.inputs.fieldOrder"
	featureTaskInputsFrom                 FeatureID = "task.inputsFrom"
	featureTaskInputsFromQualified        FeatureID = "task.inputsFrom.stageQualified"
	featureTaskCapabilities               FeatureID = "task.capabilities"
	featureTaskMinimumIntegrity           FeatureID = "task.minimumIntegrity"
	featureTaskContextFrom                FeatureID = "task.contextFrom"
	featureTaskPolicyActions              FeatureID = "task.policyActions"
	featureTaskNestedAgentPolicy          FeatureID = "task.nestedAgentPolicy"
	featureTaskRetry                      FeatureID = "task.retry"
	featureTaskRetryMaxAttempts           FeatureID = "task.retry.maxAttempts"
	featureTaskRetryBackoff               FeatureID = "task.retry.backoff"
	featureTaskTimeoutSeconds             FeatureID = "task.timeoutSeconds"
	featureTaskLimits                     FeatureID = "task.limits"
	featureTaskLimitMaxDurationSeconds    FeatureID = "task.limits.maxDurationSeconds"
	featureTaskLimitMaxTokens             FeatureID = "task.limits.maxTokens"
	featureTaskLimitMaxCostUSD            FeatureID = "task.limits.maxCostUSD"
	featureTaskTimeoutFail                FeatureID = "task.onTimeout.fail"
	featureTaskTimeoutSalvage             FeatureID = "task.onTimeout.salvage"
	featureTaskExpectedOutputs            FeatureID = "task.expectedOutputs"
	featureTaskContinueOnError            FeatureID = "task.continueOnError"
	featureTaskNext                       FeatureID = "task.next"
	featureStageShell                     FeatureID = "stage.shell"
	featureStageCIPoll                    FeatureID = "stage.ci-poll"
	featureStageExternalTelemetry         FeatureID = "stage.external-telemetry"
	featureStageCommand                   FeatureID = "stage.run.command"
	featureStageScript                    FeatureID = "stage.run.script"
	featureStageEnv                       FeatureID = "stage.run.env"
	featureStageNetworkNone               FeatureID = "stage.run.network.none"
	featureStageWorkspaceRepo             FeatureID = "stage.run.workspace.repo"
	featureStageWorkspaceScratch          FeatureID = "stage.run.workspace.scratch"
	featureStageWorkspaceRepoReadOnly     FeatureID = "stage.workspace.repo-readonly"
	featureStageWorkspace                 FeatureID = "stage.workspace"
	featureGateAgenticWorkspace           FeatureID = "gate.evaluator.agentic.workspace"
	featureStageSyncBase                  FeatureID = "stage.run.syncBase"
	featureStageResultFile                FeatureID = "stage.resultFile"
	featureGateName                       FeatureID = "gate.name"
	featureGateBranches                   FeatureID = "gate.branches"
	featureGateEscalationBranch           FeatureID = "gate.branch.escalate"
	featureEvaluatorAutomated             FeatureID = "gate.evaluator.automated"
	featureEvaluatorAutomatedCheck        FeatureID = "gate.evaluator.automated.check"
	featureEvaluatorAutomatedParams       FeatureID = "gate.evaluator.automated.params"
	featureEvaluatorAutomatedTimeout      FeatureID = "gate.evaluator.automated.timeoutSeconds"
	featureEvaluatorAutomatedRetry        FeatureID = "gate.evaluator.automated.retry"
	featureEvaluatorAutomatedRetryMax     FeatureID = "gate.evaluator.automated.retry.maxAttempts"
	featureEvaluatorAutomatedRetryBackoff FeatureID = "gate.evaluator.automated.retry.backoff"
	featureEvaluatorAutomatedPoll         FeatureID = "gate.evaluator.automated.pollIntervalSeconds"
	featureEvaluatorStatusEquals          FeatureID = "gate.evaluator.automated.check.status-equals"
	featureEvaluatorFailureClass          FeatureID = "gate.evaluator.automated.check.failure-class"
	featureEvaluatorOutputEquals          FeatureID = "gate.evaluator.automated.check.output-equals"
	featureEvaluatorOutputNotEquals       FeatureID = "gate.evaluator.automated.check.output-not-equals"
	featureEvaluatorOutputNumericGTE      FeatureID = "gate.evaluator.automated.check.output-numeric-gte"
	featureEvaluatorOutputNumericLTE      FeatureID = "gate.evaluator.automated.check.output-numeric-lte"
	featureEvaluatorOutputNumericLT       FeatureID = "gate.evaluator.automated.check.output-numeric-lt"
	featureEvaluatorOutputMatches         FeatureID = "gate.evaluator.automated.check.output-matches"
	featureEvaluatorCIStatus              FeatureID = "gate.evaluator.automated.check.ci-status"
	featureEvaluatorLandOutcome           FeatureID = "gate.evaluator.automated.check.land-outcome"
	featureEvaluatorQueueOutcome          FeatureID = "gate.evaluator.automated.check.queue-outcome"
	featureEvaluatorAgentic               FeatureID = "gate.evaluator.agentic"
	featureEvaluatorAgenticGoober         FeatureID = "gate.evaluator.agentic.goober"
	featureEvaluatorAgenticTimeout        FeatureID = "gate.evaluator.agentic.timeoutSeconds"
	featureEvaluatorAgenticRetry          FeatureID = "gate.evaluator.agentic.retry"
	featureEvaluatorAgenticRetryMax       FeatureID = "gate.evaluator.agentic.retry.maxAttempts"
	featureEvaluatorAgenticRetryBackoff   FeatureID = "gate.evaluator.agentic.retry.backoff"
	featureEvaluatorHuman                 FeatureID = "gate.evaluator.human"
	featureEvaluatorHumanApprovers        FeatureID = "gate.evaluator.human.approvers"
	featureEvaluatorHumanTimeout          FeatureID = "gate.evaluator.human.timeout"
	featureEvaluatorHumanTimeoutRemind    FeatureID = "gate.evaluator.human.onTimeout.remind"
	featureEvaluatorHumanTimeoutEscalate  FeatureID = "gate.evaluator.human.onTimeout.escalate"
	featureEvaluatorHumanTimeoutReject    FeatureID = "gate.evaluator.human.onTimeout.reject"

	// The #3292 backfill: every author-facing field of the versioned YAML
	// contract is registered — the PO-ruled reversal of #3003's
	// operational-metadata exclusion. All of these fields shipped long before
	// the registry covered them; registering them here expands registry
	// coverage of long-shipped surface, it does not add new DSL behavior.
	featureWorkflowDocsRoots                    FeatureID = "workflow.spec.docsRoots"
	featureWorkflowOutboxMirrorPath             FeatureID = "workflow.spec.outboxMirrorPath"
	featureWorkflowRequiresCapabilities         FeatureID = "workflow.spec.requires.capabilities"
	featureWorkflowTutorScope                   FeatureID = "workflow.spec.tutorScope"
	featureWorkflowTutorScopeTier               FeatureID = "workflow.spec.tutorScope.tier"
	featureWorkflowTutorScopeTarget             FeatureID = "workflow.spec.tutorScope.target"
	featureWorkflowRunControlsMaxRepasses       FeatureID = "workflow.spec.runControls.maxRepasses"
	featureWorkflowRunControlsStalledRunTimeout FeatureID = "workflow.spec.runControls.stalledRunTimeout"
	featureWorkflowRunControlsMaxRunDuration    FeatureID = "workflow.spec.runControls.maxRunDuration"
	featureTaskOutbox                           FeatureID = "task.outbox"
	featureTaskOutboxMirrorPath                 FeatureID = "task.outboxMirrorPath"
	featureTaskRequiredCapabilities             FeatureID = "task.requiredCapabilities"
	featureTriggerPriority                      FeatureID = "trigger.priority"
	featureTriggerIdleBackoff                   FeatureID = "trigger.idleBackoff"
	featureTriggerIdleBackoffEnabled            FeatureID = "trigger.idleBackoff.enabled"
	featureTriggerIdleBackoffFloor              FeatureID = "trigger.idleBackoff.floor"
	featureTriggerIdleBackoffCeiling            FeatureID = "trigger.idleBackoff.ceiling"
	featureGateMaxRepasses                      FeatureID = "gate.maxRepasses"
	featureGooberPolicyActions                  FeatureID = "goober.spec.policyActions"
	featureGooberConditionalPolicyActions       FeatureID = "goober.spec.conditionalPolicyActions"
	featureGaggleDisplayName                    FeatureID = "gaggle.spec.displayName"
	featureGaggleSelfIdentity                   FeatureID = "gaggle.spec.selfIdentity"
	featureGaggleProject                        FeatureID = "gaggle.spec.project"
	featureGaggleProjectProviderGitHub          FeatureID = "gaggle.spec.project.provider.github"
	featureGaggleProjectProviderADO             FeatureID = "gaggle.spec.project.provider.ado"
	featureGaggleProjectProviderGitea           FeatureID = "gaggle.spec.project.provider.gitea"
	featureGaggleProjectBaseURL                 FeatureID = "gaggle.spec.project.baseUrl"
	featureGaggleBacklog                        FeatureID = "gaggle.spec.backlog"
	featureGaggleBacklogProviderGitHub          FeatureID = "gaggle.spec.backlog.provider.github"
	featureGaggleBacklogProviderADO             FeatureID = "gaggle.spec.backlog.provider.ado"
	featureGaggleBacklogProviderGitea           FeatureID = "gaggle.spec.backlog.provider.gitea"
	featureGaggleBacklogBaseURL                 FeatureID = "gaggle.spec.backlog.baseUrl"
	featureGaggleBacklogLabels                  FeatureID = "gaggle.spec.backlog.labels"
	featureGaggleBacklogLabelPredicate          FeatureID = "gaggle.spec.backlog.labelPredicate"
	featureGaggleBacklogFieldPredicate          FeatureID = "gaggle.spec.backlog.fieldPredicate"
	featureGaggleIsolationNamespace             FeatureID = "gaggle.spec.isolation.namespace"
	featureGaggleIsolationIdentityRef           FeatureID = "gaggle.spec.isolation.identityRef"
	featureGaggleAdditionalRepos                FeatureID = "gaggle.spec.additionalRepos"
	featureGaggleAdditionalReposProviderGitHub  FeatureID = "gaggle.spec.additionalRepos.provider.github"
	featureGaggleAdditionalReposProviderADO     FeatureID = "gaggle.spec.additionalRepos.provider.ado"
	featureGaggleAdditionalReposProviderGitea   FeatureID = "gaggle.spec.additionalRepos.provider.gitea"
	featureGaggleAdditionalReposBaseURL         FeatureID = "gaggle.spec.additionalRepos.baseUrl"
	featureGaggleAdditionalReposCheckoutSparse  FeatureID = "gaggle.spec.additionalRepos.checkout.sparse"
	featureGaggleCICommand                      FeatureID = "gaggle.spec.ciCommand"
	featureGaggleRequiredCapabilities           FeatureID = "gaggle.spec.requiredCapabilities"
	featureGaggleBranchNamespace                FeatureID = "gaggle.spec.branchNamespace"
	featureGaggleRunControls                    FeatureID = "gaggle.spec.runControls"
	featureGaggleRunControlsMaxRepasses         FeatureID = "gaggle.spec.runControls.maxRepasses"
	featureGaggleRunControlsStalledRunTimeout   FeatureID = "gaggle.spec.runControls.stalledRunTimeout"
	featureGaggleRunControlsMaxRunDuration      FeatureID = "gaggle.spec.runControls.maxRunDuration"
	featureGaggleOutboxMirrorPath               FeatureID = "gaggle.spec.outboxMirrorPath"
	featureGaggleWorkcopiesRoot                 FeatureID = "gaggle.spec.workcopies.root"
	featureGaggleRequireLabels                  FeatureID = "gaggle.spec.requireLabels"
	featureGaggleSiblings                       FeatureID = "gaggle.spec.siblings"
)

// The registry predates the first tagged release. Keep this historical value
// fixed: build-time version metadata changes on every release, while a feature's
// since-version must not.
const initialFeatureSinceVersion = "dev"

var currentFeatureRegistry = mustFeatureRegistry(currentFeatures(initialFeatureSinceVersion))

func mustFeatureRegistry(features []Feature) FeatureRegistry {
	registry, err := NewFeatureRegistry(features)
	if err != nil {
		panic(err)
	}
	return registry
}

func currentFeatures(sinceVersion string) []Feature {
	ids := []FeatureID{
		featureWorkflowGaggle,
		featureWorkflowDisplayName,
		featureWorkflowTriggers,
		featureWorkflowReadiness,
		featureWorkflowRunControls,
		featureWorkflowMaxConcurrentRuns,
		featureWorkflowMaxRunsPerHour,
		featureWorkflowMaxRunsPerDay,
		featureWorkflowMaxChainDepth,
		featureWorkflowMaxOpenPRs,
		featureWorkflowStart,
		featureWorkflowTasks,
		featureWorkflowGates,
		featureWorkflowParallels,
		featureParallelFailurePolicy,
		featureParallelBranches,
		featureParallelJoin,
		featureParallelOnFailure,
		featureParallelBranchTimeout,
		featureParallelMaxConcurrentBranches,
		featureWorkflowTerminalComplete,
		featureWorkflowTerminalAbort,
		featureWorkflowTerminalEscalate,
		featureGaggleSandbox,
		featureGaggleCheckoutSparse,
		featureGooberGaggle,
		featureGooberRole,
		featureGooberDisplayName,
		featureGooberInstructions,
		featureGooberHarnessCopilot,
		featureGooberHarnessClaudeCode,
		featureGooberModel,
		featureGooberHarnessOptions,
		featureGooberTimeoutSeconds,
		featureGooberCapabilities,
		featureGooberSkills,
		featureGooberTools,
		featureGooberMCPServers,
		featureGooberScaleFactor,
		featureGooberWorkflows,
		featureTriggerManual,
		featureTriggerBacklogItem,
		featureTriggerBacklogItemSelector,
		featureTriggerBacklogItemTrustLabel,
		featureTriggerLabelPredicate,
		featureTriggerFieldPredicate,
		featureTriggerSchedule,
		featureTriggerSignal,
		featureTriggerWebhook,
		featureTaskName,
		featureTaskDeterministic,
		featureTaskAgentic,
		featureTaskGoal,
		featureTaskGoober,
		featureTaskInputs,
		featureTaskInputFieldOrder,
		featureTaskInputsFrom,
		featureTaskInputsFromQualified,
		featureTaskCapabilities,
		featureTaskMinimumIntegrity,
		featureTaskContextFrom,
		featureTaskPolicyActions,
		featureTaskNestedAgentPolicy,
		featureTaskRetry,
		featureTaskRetryMaxAttempts,
		featureTaskRetryBackoff,
		featureTaskTimeoutSeconds,
		featureTaskLimits,
		featureTaskLimitMaxDurationSeconds,
		featureTaskLimitMaxTokens,
		featureTaskLimitMaxCostUSD,
		featureTaskTimeoutFail,
		featureTaskTimeoutSalvage,
		featureTaskExpectedOutputs,
		featureTaskContinueOnError,
		featureTaskNext,
		featureStageShell,
		featureStageCIPoll,
		featureStageExternalTelemetry,
		featureStageCommand,
		featureStageScript,
		featureStageEnv,
		featureStageNetworkNone,
		featureStageWorkspaceRepo,
		featureStageWorkspaceScratch,
		featureStageWorkspaceRepoReadOnly,
		featureStageWorkspace,
		featureGateAgenticWorkspace,
		featureStageSyncBase,
		featureStageResultFile,
		featureGateName,
		featureGateBranches,
		featureGateEscalationBranch,
		featureEvaluatorAutomated,
		featureEvaluatorAutomatedCheck,
		featureEvaluatorAutomatedParams,
		featureEvaluatorAutomatedTimeout,
		featureEvaluatorAutomatedRetry,
		featureEvaluatorAutomatedRetryMax,
		featureEvaluatorAutomatedRetryBackoff,
		featureEvaluatorAutomatedPoll,
		featureEvaluatorStatusEquals,
		featureEvaluatorFailureClass,
		featureEvaluatorOutputEquals,
		featureEvaluatorOutputNotEquals,
		featureEvaluatorOutputNumericGTE,
		featureEvaluatorOutputNumericLTE,
		featureEvaluatorOutputNumericLT,
		featureEvaluatorOutputMatches,
		featureEvaluatorCIStatus,
		featureEvaluatorLandOutcome,
		featureEvaluatorQueueOutcome,
		featureEvaluatorAgentic,
		featureEvaluatorAgenticGoober,
		featureEvaluatorAgenticTimeout,
		featureEvaluatorAgenticRetry,
		featureEvaluatorAgenticRetryMax,
		featureEvaluatorAgenticRetryBackoff,
		featureEvaluatorHuman,
		featureEvaluatorHumanApprovers,
		featureEvaluatorHumanTimeout,
		featureEvaluatorHumanTimeoutRemind,
		featureEvaluatorHumanTimeoutEscalate,
		featureEvaluatorHumanTimeoutReject,
		featureWorkflowDocsRoots,
		featureWorkflowOutboxMirrorPath,
		featureWorkflowRequiresCapabilities,
		featureWorkflowTutorScope,
		featureWorkflowTutorScopeTier,
		featureWorkflowTutorScopeTarget,
		featureWorkflowRunControlsMaxRepasses,
		featureWorkflowRunControlsStalledRunTimeout,
		featureWorkflowRunControlsMaxRunDuration,
		featureTaskOutbox,
		featureTaskOutboxMirrorPath,
		featureTaskRequiredCapabilities,
		featureTriggerPriority,
		featureTriggerIdleBackoff,
		featureTriggerIdleBackoffEnabled,
		featureTriggerIdleBackoffFloor,
		featureTriggerIdleBackoffCeiling,
		featureGateMaxRepasses,
		featureGooberPolicyActions,
		featureGooberConditionalPolicyActions,
		featureGaggleDisplayName,
		featureGaggleSelfIdentity,
		featureGaggleProject,
		featureGaggleProjectProviderGitHub,
		featureGaggleProjectProviderADO,
		featureGaggleProjectProviderGitea,
		featureGaggleProjectBaseURL,
		featureGaggleBacklog,
		featureGaggleBacklogProviderGitHub,
		featureGaggleBacklogProviderADO,
		featureGaggleBacklogProviderGitea,
		featureGaggleBacklogBaseURL,
		featureGaggleBacklogLabels,
		featureGaggleBacklogLabelPredicate,
		featureGaggleBacklogFieldPredicate,
		featureGaggleIsolationNamespace,
		featureGaggleIsolationIdentityRef,
		featureGaggleAdditionalRepos,
		featureGaggleAdditionalReposProviderGitHub,
		featureGaggleAdditionalReposProviderADO,
		featureGaggleAdditionalReposProviderGitea,
		featureGaggleAdditionalReposBaseURL,
		featureGaggleAdditionalReposCheckoutSparse,
		featureGaggleCICommand,
		featureGaggleRequiredCapabilities,
		featureGaggleBranchNamespace,
		featureGaggleRunControls,
		featureGaggleRunControlsMaxRepasses,
		featureGaggleRunControlsStalledRunTimeout,
		featureGaggleRunControlsMaxRunDuration,
		featureGaggleOutboxMirrorPath,
		featureGaggleWorkcopiesRoot,
		featureGaggleRequireLabels,
		featureGaggleSiblings,
	}
	features := make([]Feature, 0, len(ids))
	for _, id := range ids {
		if promotedVersion, promoted := gaPromotions[id]; promoted {
			features = append(features, Feature{
				ID:           id,
				Level:        SupportGA,
				SinceVersion: promotedVersion,
				DSLVersions: []DSLFeatureSupport{{
					Version: DSLVersion,
					Level:   SupportGA,
				}},
				History: []SupportTransition{
					{Level: SupportPreview, SinceVersion: sinceVersion},
					{Level: SupportGA, SinceVersion: promotedVersion},
				},
			})
			continue
		}
		level := SupportGA
		if _, preview := previewFeatures[id]; preview {
			level = SupportPreview
		}
		features = append(features, Feature{
			ID:           id,
			Level:        level,
			SinceVersion: sinceVersion,
			DSLVersions: []DSLFeatureSupport{{
				Version: DSLVersion,
				Level:   level,
			}},
			History: []SupportTransition{{
				Level:        level,
				SinceVersion: sinceVersion,
			}},
		})
	}
	return features
}

// gaPromotions records features that entered the registry at preview in a
// released version and have since graduated to GA. Unlike the pre-release
// surface (which is GA in a single "dev" transition), a promoted feature keeps
// its released preview baseline as history and appends the ga transition at
// the release that promotes it, which is exactly the preview -> ga advance
// validateFeatureRegistryEvolution accepts against the released snapshot.
//
// task.inputsFrom.stageQualified (#562) shipped at preview in v0.1.0 and rides
// the 2.0 lock ceremony to GA (#3292 ruling): the runner has executed
// stage-qualified handoffs since the FO-8 conformance corpus went green, so
// "unproven" no longer applies. v0.2.0 is the lock-ceremony release the
// promotion lands in.
var gaPromotions = map[FeatureID]string{
	featureTaskInputsFromQualified: "v0.2.0",
}

// previewFeatures lists the DSL features that remain preview — unstable and
// gated behind the explicit goobers.dev/allow-preview-features opt-in — at the
// current pre-release ("dev") version. Everything else in the canonical DSL
// surface is GA: the shipped, documented language that guided-init scaffolds
// and config-examples model, which must validate without a preview
// acknowledgement (an earlier placeholder marked *every* field preview, so
// guided-init tripped VER002 on every standard field, #1196). Only genuinely
// unproven features stay preview: the per-gaggle sandbox posture override,
// whose enforcement is landing behind the default-off instance opt-in
// (#1305). Sparse checkout (featureGaggleCheckoutSparse) promoted to GA once
// the local runner started honoring it (#649) — it was preview only because
// it was "accepted but inert"; declaring it is now a real, opt-in behavior
// change, exactly the case GA features already cover.
// Promoting a feature to GA is a one-line removal from this map.
var previewFeatures = map[FeatureID]struct{}{
	featureGaggleSandbox: {},
	// Stage-qualified inputsFrom (#562) entered preview like every other new
	// DSL surface and graduated to GA with the 2.0 lock (see gaPromotions):
	// the runner executes stage-qualified handoffs, so it is no longer
	// "accepted but inert". The per-gaggle sandbox posture override above
	// stays preview in 2.0 by explicit ruling — its enforcement remains
	// behind the default-off instance opt-in (#1305).
	// Static fan-out/fan-in and the read-only repo workspace / task/gate-level
	// workspace seam it motivated (#1562) graduated to GA once FO-8's
	// conformance corpus went green (#1566, internal/runner/
	// parallel_conformance_test.go) — the runner now genuinely executes a
	// parallel (bounded concurrency, branch timeout, failure-policy routing,
	// crash recovery), so these are no longer "DSL surface with the runner
	// still refusing to execute it."
}

type featureSet map[FeatureID]struct{}

func (s featureSet) add(ids ...FeatureID) {
	for _, id := range ids {
		s[id] = struct{}{}
	}
}

func (s featureSet) ids() []FeatureID {
	ids := make([]FeatureID, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids
}

type retryFeatureIDs struct {
	policy      FeatureID
	maxAttempts FeatureID
	backoff     FeatureID
}

func addNestedAgentPolicyFeatures(used featureSet, policy *apiv1.NestedAgentPolicy) {
	if policy != nil {
		used.add(featureTaskNestedAgentPolicy)
	}
}

func addRetryFeatures(used featureSet, retry *apiv1.RetryPolicy, ids retryFeatureIDs) {
	if retry == nil {
		return
	}
	used.add(ids.policy, ids.maxAttempts)
	if retry.BackoffSeconds != 0 {
		used.add(ids.backoff)
	}
}

// runControlFeatureIDs parameterizes run-control resolution per declaration
// site (workflow vs gaggle), mirroring retryFeatureIDs, so the two sites
// cannot drift in spelling.
type runControlFeatureIDs struct {
	controls          FeatureID
	maxRepasses       FeatureID
	stalledRunTimeout FeatureID
	maxRunDuration    FeatureID
}

var workflowRunControlFeatureIDs = runControlFeatureIDs{
	controls:          featureWorkflowRunControls,
	maxRepasses:       featureWorkflowRunControlsMaxRepasses,
	stalledRunTimeout: featureWorkflowRunControlsStalledRunTimeout,
	maxRunDuration:    featureWorkflowRunControlsMaxRunDuration,
}

var gaggleRunControlFeatureIDs = runControlFeatureIDs{
	controls:          featureGaggleRunControls,
	maxRepasses:       featureGaggleRunControlsMaxRepasses,
	stalledRunTimeout: featureGaggleRunControlsStalledRunTimeout,
	maxRunDuration:    featureGaggleRunControlsMaxRunDuration,
}

func addRunControlFeatures(used featureSet, controls *apiv1.RunControls, ids runControlFeatureIDs) {
	if controls == nil {
		return
	}
	used.add(ids.controls)
	if controls.MaxRepasses != 0 {
		used.add(ids.maxRepasses)
	}
	if controls.StalledRunTimeout != "" {
		used.add(ids.stalledRunTimeout)
	}
	if controls.MaxRunDuration != "" {
		used.add(ids.maxRunDuration)
	}
}

// providerFeatureIDs parameterizes the per-enum-value provider features per
// declaration site (project, backlog, additionalRepos), so the sites cannot
// drift in spelling. Provider enum values select distinct implementations
// (internal/bootstrap/providers.go switches on them), so each value carries
// its own FeatureID, following the goober.spec.harness.* precedent.
type providerFeatureIDs struct {
	github FeatureID
	ado    FeatureID
	gitea  FeatureID
}

var gaggleProjectProviderFeatureIDs = providerFeatureIDs{
	github: featureGaggleProjectProviderGitHub,
	ado:    featureGaggleProjectProviderADO,
	gitea:  featureGaggleProjectProviderGitea,
}

var gaggleBacklogProviderFeatureIDs = providerFeatureIDs{
	github: featureGaggleBacklogProviderGitHub,
	ado:    featureGaggleBacklogProviderADO,
	gitea:  featureGaggleBacklogProviderGitea,
}

var gaggleAdditionalRepoProviderFeatureIDs = providerFeatureIDs{
	github: featureGaggleAdditionalReposProviderGitHub,
	ado:    featureGaggleAdditionalReposProviderADO,
	gitea:  featureGaggleAdditionalReposProviderGitea,
}

func addProviderFeature(used featureSet, provider apiv1.Provider, ids providerFeatureIDs) {
	switch provider {
	case apiv1.ProviderGitHub:
		used.add(ids.github)
	case apiv1.ProviderADO:
		used.add(ids.ado)
	case apiv1.ProviderGitea:
		used.add(ids.gitea)
	}
}

// backlogRefDeclared reports whether any field of the backlog reference is
// set. BacklogRef contains slices, so it cannot be compared to its zero value
// the way RepoRef can.
func backlogRefDeclared(ref apiv1.BacklogRef) bool {
	return ref.Provider != "" || ref.BaseURL != "" || ref.Project != "" ||
		ref.Labels != nil || ref.LabelPredicate != "" || ref.FieldPredicate != "" ||
		ref.ConnectionRef != ""
}

// FeaturesForWorkflow returns registry metadata for the DSL features used by
// def. VER-2 consumes the returned levels to enforce compatibility policy.
func FeaturesForWorkflow(def Definition) ([]Feature, error) {
	used := featureSet{}
	used.add(
		featureWorkflowGaggle,
		featureWorkflowTriggers,
		featureWorkflowReadiness,
		featureWorkflowMaxConcurrentRuns,
		featureWorkflowMaxRunsPerHour,
		featureWorkflowStart,
	)
	if def.Spec.DisplayName != "" {
		used.add(featureWorkflowDisplayName)
	}
	if def.Spec.Readiness.MaxRunsPerDay != 0 {
		used.add(featureWorkflowMaxRunsPerDay)
	}
	if def.Spec.Readiness.MaxChainDepth != 0 {
		used.add(featureWorkflowMaxChainDepth)
	}
	if def.Spec.Readiness.MaxOpenPRs != 0 {
		used.add(featureWorkflowMaxOpenPRs)
	}
	addRunControlFeatures(used, def.Spec.RunControls, workflowRunControlFeatureIDs)
	if def.Spec.OutboxMirrorPath != "" {
		used.add(featureWorkflowOutboxMirrorPath)
	}
	if def.Spec.DocsRoots != nil {
		used.add(featureWorkflowDocsRoots)
	}
	if def.Spec.TutorScope != nil {
		used.add(featureWorkflowTutorScope)
		if def.Spec.TutorScope.Tier != "" {
			used.add(featureWorkflowTutorScopeTier)
		}
		if def.Spec.TutorScope.Target != "" {
			used.add(featureWorkflowTutorScopeTarget)
		}
	}
	if def.Spec.Requires != nil {
		used.add(featureWorkflowRequiresCapabilities)
	}
	for _, trigger := range def.Spec.Triggers {
		addTriggerFeatures(used, trigger)
	}
	if def.Spec.Tasks != nil {
		used.add(featureWorkflowTasks)
	}
	for _, task := range def.Spec.Tasks {
		addTaskFeatures(used, task)
	}
	if usesStageQualifiedInputs(def) {
		used.add(featureTaskInputsFromQualified)
	}
	if def.Spec.Gates != nil {
		used.add(featureWorkflowGates)
	}
	for _, gate := range def.Spec.Gates {
		addGateFeatures(used, gate)
	}
	if def.Spec.Parallels != nil {
		used.add(featureWorkflowParallels)
	}
	for _, parallel := range def.Spec.Parallels {
		addParallelFeatures(used, parallel)
	}
	return currentFeatureRegistry.resolve(used.ids())
}

// FeaturesForGaggle returns registry metadata for the DSL features used by
// spec. Every add is guarded on the field actually being set: a zero field
// must never pull its feature into the resolved surface (mirroring
// FeaturesForWorkflow's semantic-digest discipline).
func FeaturesForGaggle(spec apiv1.GaggleSpec) ([]Feature, error) {
	used := featureSet{}
	if spec.DisplayName != "" {
		used.add(featureGaggleDisplayName)
	}
	if spec.SelfIdentity != "" {
		used.add(featureGaggleSelfIdentity)
	}
	// Identity payload (owner/project/name/branch/connectionRef) folds into
	// the container feature; only behavior-selecting sub-fields (provider,
	// baseUrl, checkout.sparse) carry their own IDs.
	if spec.Project != (apiv1.RepoRef{}) {
		used.add(featureGaggleProject)
	}
	addProviderFeature(used, spec.Project.Provider, gaggleProjectProviderFeatureIDs)
	if spec.Project.BaseURL != "" {
		used.add(featureGaggleProjectBaseURL)
	}
	if spec.Project.Checkout != nil && spec.Project.Checkout.Sparse != nil {
		used.add(featureGaggleCheckoutSparse)
	}
	if backlogRefDeclared(spec.Backlog) {
		used.add(featureGaggleBacklog)
	}
	addProviderFeature(used, spec.Backlog.Provider, gaggleBacklogProviderFeatureIDs)
	if spec.Backlog.BaseURL != "" {
		used.add(featureGaggleBacklogBaseURL)
	}
	if spec.Backlog.Labels != nil {
		used.add(featureGaggleBacklogLabels)
	}
	if spec.Backlog.LabelPredicate != "" {
		used.add(featureGaggleBacklogLabelPredicate)
	}
	if spec.Backlog.FieldPredicate != "" {
		used.add(featureGaggleBacklogFieldPredicate)
	}
	if spec.Isolation.Namespace != "" {
		used.add(featureGaggleIsolationNamespace)
	}
	if spec.Isolation.IdentityRef != "" {
		used.add(featureGaggleIsolationIdentityRef)
	}
	if spec.AdditionalRepos != nil {
		used.add(featureGaggleAdditionalRepos)
	}
	for _, repo := range spec.AdditionalRepos {
		addProviderFeature(used, repo.Provider, gaggleAdditionalRepoProviderFeatureIDs)
		if repo.BaseURL != "" {
			used.add(featureGaggleAdditionalReposBaseURL)
		}
		// A second ID rather than a widened gaggle.spec.project.checkout.sparse
		// guard: released IDs are pinned by validateFeatureRegistryEvolution,
		// and before this ID existed a gaggle declaring sparse cones only on an
		// additionalRepos entry — honored by the runner — resolved zero features.
		if repo.Checkout != nil && repo.Checkout.Sparse != nil {
			used.add(featureGaggleAdditionalReposCheckoutSparse)
		}
	}
	if spec.CICommand != nil {
		used.add(featureGaggleCICommand)
	}
	if spec.RequiredCapabilities != nil {
		used.add(featureGaggleRequiredCapabilities)
	}
	if spec.BranchNamespace != "" {
		used.add(featureGaggleBranchNamespace)
	}
	addRunControlFeatures(used, spec.RunControls, gaggleRunControlFeatureIDs)
	if spec.OutboxMirrorPath != "" {
		used.add(featureGaggleOutboxMirrorPath)
	}
	if spec.Sandbox != nil {
		used.add(featureGaggleSandbox)
	}
	if spec.Workcopies != nil {
		used.add(featureGaggleWorkcopiesRoot)
	}
	if spec.RequireLabels != nil {
		used.add(featureGaggleRequireLabels)
	}
	if spec.Siblings != nil {
		used.add(featureGaggleSiblings)
	}
	return currentFeatureRegistry.resolve(used.ids())
}

// addParallelFeatures records the GA DSL fields used by a parallel state.
func addParallelFeatures(used featureSet, parallel apiv1.Parallel) {
	if parallel.FailurePolicy != "" {
		used.add(featureParallelFailurePolicy)
	}
	if parallel.Branches != nil {
		used.add(featureParallelBranches)
	}
	if parallel.Join != "" {
		used.add(featureParallelJoin)
	}
	if parallel.OnFailure != "" {
		used.add(featureParallelOnFailure)
	}
	if parallel.BranchTimeoutSeconds != 0 {
		used.add(featureParallelBranchTimeout)
	}
	if parallel.MaxConcurrentBranches != 0 {
		used.add(featureParallelMaxConcurrentBranches)
	}
}

// FeaturesForGoober returns registry metadata for the DSL features used by
// spec. VER-2 consumes the returned levels to enforce compatibility policy.
func FeaturesForGoober(spec apiv1.GooberSpec) ([]Feature, error) {
	used := featureSet{}
	used.add(featureGooberGaggle, featureGooberRole, featureGooberInstructions)
	if spec.DisplayName != "" {
		used.add(featureGooberDisplayName)
	}
	if spec.Harness == "" || spec.Harness == apiv1.HarnessCopilot {
		used.add(featureGooberHarnessCopilot)
	}
	if spec.Harness == apiv1.HarnessClaudeCode {
		used.add(featureGooberHarnessClaudeCode)
	}
	if spec.Model != "" {
		used.add(featureGooberModel)
	}
	if spec.HarnessOptions != nil {
		used.add(featureGooberHarnessOptions)
	}
	if spec.TimeoutSeconds > 0 {
		used.add(featureGooberTimeoutSeconds)
	}
	if spec.Capabilities != nil {
		used.add(featureGooberCapabilities)
	}
	if spec.Skills != nil {
		used.add(featureGooberSkills)
	}
	if spec.Tools != nil {
		used.add(featureGooberTools)
	}
	if spec.MCPServers != nil {
		used.add(featureGooberMCPServers)
	}
	if spec.PolicyActions != nil {
		used.add(featureGooberPolicyActions)
	}
	if spec.ConditionalPolicyActions != nil {
		used.add(featureGooberConditionalPolicyActions)
	}
	used.add(featureGooberScaleFactor)
	if spec.Workflows != nil {
		used.add(featureGooberWorkflows)
	}
	return currentFeatureRegistry.resolve(used.ids())
}

func addTriggerFeatures(used featureSet, trigger apiv1.Trigger) {
	if trigger.TrustLabel != "" {
		used.add(featureTriggerBacklogItemTrustLabel)
	}
	if trigger.LabelPredicate != "" {
		used.add(featureTriggerLabelPredicate)
	}
	if trigger.FieldPredicate != "" {
		used.add(featureTriggerFieldPredicate)
	}
	if trigger.Priority != 0 {
		used.add(featureTriggerPriority)
	}
	if trigger.IdleBackoff != nil {
		used.add(featureTriggerIdleBackoff)
		if trigger.IdleBackoff.Enabled != nil {
			used.add(featureTriggerIdleBackoffEnabled)
		}
		if trigger.IdleBackoff.Floor != "" {
			used.add(featureTriggerIdleBackoffFloor)
		}
		if trigger.IdleBackoff.Ceiling != "" {
			used.add(featureTriggerIdleBackoffCeiling)
		}
	}
	switch trigger.Type {
	case apiv1.TriggerManual:
		used.add(featureTriggerManual)
	case apiv1.TriggerBacklogItem:
		used.add(featureTriggerBacklogItem)
		if trigger.Selector != nil {
			used.add(featureTriggerBacklogItemSelector)
		}
	case apiv1.TriggerSchedule:
		used.add(featureTriggerSchedule)
	case apiv1.TriggerSignal:
		used.add(featureTriggerSignal)
	case apiv1.TriggerWebhook:
		used.add(featureTriggerWebhook)
	}
}

func addTaskFeatures(used featureSet, task apiv1.Task) {
	if task.Workspace != "" {
		used.add(featureStageWorkspace)
		if task.Workspace == apiv1.WorkspaceRepoReadOnly {
			used.add(featureStageWorkspaceRepoReadOnly)
		}
	}
	used.add(featureTaskName, featureTaskGoal)
	switch task.Type {
	case apiv1.TaskDeterministic:
		used.add(featureTaskDeterministic)
	case apiv1.TaskAgentic:
		used.add(featureTaskAgentic)
	}
	if task.Goober != "" {
		used.add(featureTaskGoober)
	}
	if task.Inputs != nil {
		used.add(featureTaskInputs)
	}
	if _, ok := task.Inputs["fieldOrder"]; ok {
		used.add(featureTaskInputFieldOrder)
	}
	if task.Inputs["resultFile"] != "" {
		used.add(featureStageResultFile)
	}
	if task.InputsFrom != nil {
		used.add(featureTaskInputsFrom)
	}
	if task.Capabilities != nil {
		used.add(featureTaskCapabilities)
	}
	if task.MinimumIntegrity != "" {
		used.add(featureTaskMinimumIntegrity)
	}
	if task.ContextFrom != nil {
		used.add(featureTaskContextFrom)
	}
	if task.PolicyActions != nil {
		used.add(featureTaskPolicyActions)
	}
	addNestedAgentPolicyFeatures(used, task.NestedAgentPolicy)
	if task.RequiredCapabilities != nil {
		used.add(featureTaskRequiredCapabilities)
	}
	if task.Outbox != nil {
		used.add(featureTaskOutbox)
	}
	if task.OutboxMirrorPath != "" {
		used.add(featureTaskOutboxMirrorPath)
	}
	addRetryFeatures(used, task.Retry, retryFeatureIDs{
		policy:      featureTaskRetry,
		maxAttempts: featureTaskRetryMaxAttempts,
		backoff:     featureTaskRetryBackoff,
	})
	if task.TimeoutSeconds != 0 {
		used.add(featureTaskTimeoutSeconds)
	}
	if task.Limits != nil {
		used.add(featureTaskLimits)
		if task.Limits.MaxDurationSeconds != 0 {
			used.add(featureTaskLimitMaxDurationSeconds)
		}
		if task.Limits.MaxTokens != 0 {
			used.add(featureTaskLimitMaxTokens)
		}
		if task.Limits.MaxCostUSD != 0 {
			used.add(featureTaskLimitMaxCostUSD)
		}
	}
	switch task.OnTimeout {
	case apiv1.TaskOnTimeoutFail:
		used.add(featureTaskTimeoutFail)
	case "":
		if task.Type == apiv1.TaskAgentic {
			used.add(featureTaskTimeoutFail)
		}
	case apiv1.TaskOnTimeoutSalvage:
		used.add(featureTaskTimeoutSalvage)
	}
	if task.ExpectedOutputs != nil {
		used.add(featureTaskExpectedOutputs)
	}
	if task.ContinueOnError {
		used.add(featureTaskContinueOnError)
	}
	if task.Next != "" {
		used.add(featureTaskNext)
		addTargetFeature(used, task.Next)
	} else {
		used.add(featureWorkflowTerminalComplete)
	}
	if task.Type != apiv1.TaskDeterministic || task.Run == nil {
		return
	}
	if task.Run.Script != "" {
		used.add(featureStageScript)
	} else {
		used.add(featureStageCommand)
	}
	if task.Run.Env != nil {
		used.add(featureStageEnv)
	}
	switch strings.TrimSpace(task.Inputs["kind"]) {
	case "", "shell":
		used.add(featureStageShell)
	case "ci-poll":
		used.add(featureStageCIPoll)
	case "external-telemetry":
		used.add(featureStageExternalTelemetry)
	}
	if task.Run.Network == apiv1.NetworkNone {
		used.add(featureStageNetworkNone)
	}
	switch task.Run.Workspace {
	case "", apiv1.WorkspaceRepo:
		used.add(featureStageWorkspaceRepo)
	case apiv1.WorkspaceScratch:
		used.add(featureStageWorkspaceScratch)
	case apiv1.WorkspaceRepoReadOnly:
		used.add(featureStageWorkspaceRepoReadOnly)
	}
	if task.Run.SyncBase {
		used.add(featureStageSyncBase)
	}
}

var automatedCheckFeatures = map[string]FeatureID{
	"status-equals":      featureEvaluatorStatusEquals,
	"failure-class":      featureEvaluatorFailureClass,
	"output-equals":      featureEvaluatorOutputEquals,
	"output-not-equals":  featureEvaluatorOutputNotEquals,
	"output-numeric-gte": featureEvaluatorOutputNumericGTE,
	"output-numeric-lte": featureEvaluatorOutputNumericLTE,
	"output-numeric-lt":  featureEvaluatorOutputNumericLT,
	"output-matches":     featureEvaluatorOutputMatches,
	"ci-status":          featureEvaluatorCIStatus,
	"land-outcome":       featureEvaluatorLandOutcome,
	"queue-outcome":      featureEvaluatorQueueOutcome,
}

func addGateFeatures(used featureSet, gate apiv1.Gate) {
	used.add(featureGateName, featureGateBranches)
	if gate.MaxRepasses != 0 {
		used.add(featureGateMaxRepasses)
	}
	for outcome, target := range gate.Branches {
		if outcome == BranchEscalate {
			used.add(featureGateEscalationBranch)
		}
		addTargetFeature(used, target)
	}
	switch gate.Evaluator {
	case apiv1.EvaluatorAutomated:
		used.add(featureEvaluatorAutomated)
		if gate.Automated == nil {
			return
		}
		used.add(featureEvaluatorAutomatedCheck)
		if gate.Automated.Params != nil {
			used.add(featureEvaluatorAutomatedParams)
		}
		if gate.Automated.TimeoutSeconds != 0 {
			used.add(featureEvaluatorAutomatedTimeout)
		}
		addRetryFeatures(used, gate.Automated.Retry, retryFeatureIDs{
			policy:      featureEvaluatorAutomatedRetry,
			maxAttempts: featureEvaluatorAutomatedRetryMax,
			backoff:     featureEvaluatorAutomatedRetryBackoff,
		})
		if gate.Automated.PollIntervalSeconds != 0 {
			used.add(featureEvaluatorAutomatedPoll)
		}
		if feature, ok := automatedCheckFeatures[gate.Automated.Check]; ok {
			used.add(feature)
		}
	case apiv1.EvaluatorAgentic:
		used.add(featureEvaluatorAgentic)
		if gate.Agentic == nil {
			return
		}
		if gate.Agentic.Goober != "" {
			used.add(featureEvaluatorAgenticGoober)
		}
		if gate.Agentic.TimeoutSeconds != 0 {
			used.add(featureEvaluatorAgenticTimeout)
		}
		if gate.Agentic.Workspace != "" {
			used.add(featureGateAgenticWorkspace)
			if gate.Agentic.Workspace == apiv1.WorkspaceRepoReadOnly {
				used.add(featureStageWorkspaceRepoReadOnly)
			}
		}
		addRetryFeatures(used, gate.Agentic.Retry, retryFeatureIDs{
			policy:      featureEvaluatorAgenticRetry,
			maxAttempts: featureEvaluatorAgenticRetryMax,
			backoff:     featureEvaluatorAgenticRetryBackoff,
		})
	case apiv1.EvaluatorHuman:
		used.add(featureEvaluatorHuman)
		if gate.Human == nil {
			return
		}
		if gate.Human.Approvers != nil {
			used.add(featureEvaluatorHumanApprovers)
		}
		if gate.Human.TimeoutSeconds != 0 {
			used.add(featureEvaluatorHumanTimeout)
		}
		switch gate.Human.OnTimeout {
		case "remind":
			used.add(featureEvaluatorHumanTimeoutRemind)
		case "escalate":
			used.add(featureEvaluatorHumanTimeoutEscalate)
		case "reject":
			used.add(featureEvaluatorHumanTimeoutReject)
		}
	}
}

func addTargetFeature(used featureSet, target string) {
	switch target {
	case TerminalComplete:
		used.add(featureWorkflowTerminalComplete)
	case TargetAbort:
		used.add(featureWorkflowTerminalAbort)
	case TargetEscalate:
		used.add(featureWorkflowTerminalEscalate)
	}
}

// usesStageQualifiedInputs reports whether any inputsFrom value is a
// stage-qualified reference (#562).
//
// The prefix must name a DECLARED task. A value like "legacy.dotted" whose
// prefix is not a stage is an ordinary bare output key and must not be counted
// — otherwise every workflow with a dotted output key would suddenly require
// the preview opt-in without using the feature at all.
func usesStageQualifiedInputs(def Definition) bool {
	declared := make(map[string]bool, len(def.Spec.Tasks))
	for _, task := range def.Spec.Tasks {
		declared[task.Name] = true
	}
	for _, task := range def.Spec.Tasks {
		for _, value := range task.InputsFrom {
			if stage, _, ok := splitQualifiedRef(value); ok && declared[stage] {
				return true
			}
		}
	}
	return false
}
