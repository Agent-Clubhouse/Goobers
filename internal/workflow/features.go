package workflow

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	History               []SupportTransition `json:"history"`
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
		if err := validateFeatureHistory(feature); err != nil {
			return FeatureRegistry{}, fmt.Errorf("DSL feature %q: %w", feature.ID, err)
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
	return feature
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

const (
	featureWorkflowGaggle                 FeatureID = "workflow.spec.gaggle"
	featureWorkflowDisplayName            FeatureID = "workflow.spec.displayName"
	featureWorkflowTriggers               FeatureID = "workflow.spec.triggers"
	featureWorkflowReadiness              FeatureID = "workflow.spec.readiness"
	featureWorkflowMaxConcurrentRuns      FeatureID = "workflow.spec.readiness.maxConcurrentRuns"
	featureWorkflowMaxRunsPerHour         FeatureID = "workflow.spec.readiness.maxRunsPerHour"
	featureWorkflowMaxRunsPerDay          FeatureID = "workflow.spec.readiness.maxRunsPerDay"
	featureWorkflowMaxChainDepth          FeatureID = "workflow.spec.readiness.maxChainDepth"
	featureWorkflowMaxOpenPRs             FeatureID = "workflow.spec.readiness.maxOpenPRs"
	featureWorkflowStart                  FeatureID = "workflow.spec.start"
	featureWorkflowTasks                  FeatureID = "workflow.spec.tasks"
	featureWorkflowGates                  FeatureID = "workflow.spec.gates"
	featureWorkflowTerminalComplete       FeatureID = "workflow.terminal.complete"
	featureWorkflowTerminalAbort          FeatureID = "workflow.terminal.abort"
	featureWorkflowTerminalEscalate       FeatureID = "workflow.terminal.escalate"
	featureGooberGaggle                   FeatureID = "goober.spec.gaggle"
	featureGooberRole                     FeatureID = "goober.spec.role"
	featureGooberDisplayName              FeatureID = "goober.spec.displayName"
	featureGooberInstructions             FeatureID = "goober.spec.instructions"
	featureGooberHarnessCopilot           FeatureID = "goober.spec.harness.copilot"
	featureGooberModel                    FeatureID = "goober.spec.model"
	featureGooberHarnessOptions           FeatureID = "goober.spec.harnessOptions"
	featureGooberTimeoutSeconds           FeatureID = "goober.spec.timeoutSeconds"
	featureGooberCapabilities             FeatureID = "goober.spec.capabilities"
	featureGooberSkills                   FeatureID = "goober.spec.skills"
	featureGooberTools                    FeatureID = "goober.spec.tools"
	featureGooberScaleFactor              FeatureID = "goober.spec.scaleFactor"
	featureGooberWorkflows                FeatureID = "goober.spec.workflows"
	featureTriggerManual                  FeatureID = "trigger.manual"
	featureTriggerBacklogItem             FeatureID = "trigger.backlog-item"
	featureTriggerBacklogItemSelector     FeatureID = "trigger.backlog-item.selector"
	featureTriggerSchedule                FeatureID = "trigger.schedule"
	featureTriggerSignal                  FeatureID = "trigger.signal"
	featureTriggerWebhook                 FeatureID = "trigger.webhook"
	featureTaskName                       FeatureID = "task.name"
	featureTaskDeterministic              FeatureID = "task.deterministic"
	featureTaskAgentic                    FeatureID = "task.agentic"
	featureTaskGoal                       FeatureID = "task.goal"
	featureTaskGoober                     FeatureID = "task.goober"
	featureTaskInputs                     FeatureID = "task.inputs"
	featureTaskInputsFrom                 FeatureID = "task.inputsFrom"
	featureTaskCapabilities               FeatureID = "task.capabilities"
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
	featureStageCommand                   FeatureID = "stage.run.command"
	featureStageEnv                       FeatureID = "stage.run.env"
	featureStageImage                     FeatureID = "stage.run.image"
	featureStageNetworkNone               FeatureID = "stage.run.network.none"
	featureStageWorkspaceRepo             FeatureID = "stage.run.workspace.repo"
	featureStageWorkspaceScratch          FeatureID = "stage.run.workspace.scratch"
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
		featureWorkflowMaxConcurrentRuns,
		featureWorkflowMaxRunsPerHour,
		featureWorkflowMaxRunsPerDay,
		featureWorkflowMaxChainDepth,
		featureWorkflowMaxOpenPRs,
		featureWorkflowStart,
		featureWorkflowTasks,
		featureWorkflowGates,
		featureWorkflowTerminalComplete,
		featureWorkflowTerminalAbort,
		featureWorkflowTerminalEscalate,
		featureGooberGaggle,
		featureGooberRole,
		featureGooberDisplayName,
		featureGooberInstructions,
		featureGooberHarnessCopilot,
		featureGooberModel,
		featureGooberHarnessOptions,
		featureGooberTimeoutSeconds,
		featureGooberCapabilities,
		featureGooberSkills,
		featureGooberTools,
		featureGooberScaleFactor,
		featureGooberWorkflows,
		featureTriggerManual,
		featureTriggerBacklogItem,
		featureTriggerBacklogItemSelector,
		featureTriggerSchedule,
		featureTriggerSignal,
		featureTriggerWebhook,
		featureTaskName,
		featureTaskDeterministic,
		featureTaskAgentic,
		featureTaskGoal,
		featureTaskGoober,
		featureTaskInputs,
		featureTaskInputsFrom,
		featureTaskCapabilities,
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
		featureStageCommand,
		featureStageEnv,
		featureStageImage,
		featureStageNetworkNone,
		featureStageWorkspaceRepo,
		featureStageWorkspaceScratch,
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
	}
	features := make([]Feature, 0, len(ids))
	for _, id := range ids {
		history := []SupportTransition{{
			Level:        SupportPreview,
			SinceVersion: sinceVersion,
		}}
		features = append(features, Feature{
			ID:           id,
			Level:        SupportPreview,
			SinceVersion: sinceVersion,
			History:      history,
		})
	}
	return features
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

func addRetryFeatures(used featureSet, retry *apiv1.RetryPolicy, ids retryFeatureIDs) {
	if retry == nil {
		return
	}
	used.add(ids.policy, ids.maxAttempts)
	if retry.BackoffSeconds != 0 {
		used.add(ids.backoff)
	}
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
	for _, trigger := range def.Spec.Triggers {
		addTriggerFeatures(used, trigger)
	}
	if def.Spec.Tasks != nil {
		used.add(featureWorkflowTasks)
	}
	for _, task := range def.Spec.Tasks {
		addTaskFeatures(used, task)
	}
	if def.Spec.Gates != nil {
		used.add(featureWorkflowGates)
	}
	for _, gate := range def.Spec.Gates {
		addGateFeatures(used, gate)
	}
	return currentFeatureRegistry.resolve(used.ids())
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
	used.add(featureGooberScaleFactor)
	if spec.Workflows != nil {
		used.add(featureGooberWorkflows)
	}
	return currentFeatureRegistry.resolve(used.ids())
}

func addTriggerFeatures(used featureSet, trigger apiv1.Trigger) {
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
	if task.Inputs["resultFile"] != "" {
		used.add(featureStageResultFile)
	}
	if task.InputsFrom != nil {
		used.add(featureTaskInputsFrom)
	}
	if task.Capabilities != nil {
		used.add(featureTaskCapabilities)
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
	used.add(featureStageCommand)
	if task.Run.Env != nil {
		used.add(featureStageEnv)
	}
	switch strings.TrimSpace(task.Inputs["kind"]) {
	case "", "shell":
		used.add(featureStageShell)
	case "ci-poll":
		used.add(featureStageCIPoll)
	}
	if task.Run.Image != "" {
		used.add(featureStageImage)
	}
	if task.Run.Network == apiv1.NetworkNone {
		used.add(featureStageNetworkNone)
	}
	switch task.Run.Workspace {
	case "", apiv1.WorkspaceRepo:
		used.add(featureStageWorkspaceRepo)
	case apiv1.WorkspaceScratch:
		used.add(featureStageWorkspaceScratch)
	}
	if task.Run.SyncBase {
		used.add(featureStageSyncBase)
	}
}

var automatedCheckFeatures = map[string]FeatureID{
	"status-equals":      featureEvaluatorStatusEquals,
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
