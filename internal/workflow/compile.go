package workflow

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/supportmatrix"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
	vnext "github.com/goobers/goobers/internal/workflow/v_next"
)

type versionedInterpreter struct {
	compile                         func(Definition, compileConfig) (*Machine, error)
	checkWarnings                   func(Definition) []string
	checkReachability               func(Definition) []string
	checkSchedules                  func(Definition) []string
	checkTriggerFields              func(Definition) []string
	checkWorkflowAdmission          func(Definition, map[string]apiv1.GooberSpec) []string
	checkPushBoundaries             func(Definition, []string) []string
	checkRunsOnOSTokens             func(Definition, *apiv1.GaggleRunsOn) []string
	checkRunsOnRestrictions         func(Definition, *apiv1.GaggleRunsOn) []string
	checkRunsOnPlacement            func(Definition, *apiv1.GaggleRunsOn) []string
	checkRepoHandoffs               func(Definition) []string
	checkGateParameters             func(Definition) []string
	checkGateOutcomes               func(Definition) []string
	checkStageRequiredInputs        func(Definition) []string
	checkStageContracts             func(Definition) []string
	checkStageContractWarnings      func(Definition) []string
	checkStageTimeoutCoherence      func(Definition) []string
	checkSubprocessTimeoutCoherence func(Definition) []string
	checkPathSimulation             func(Definition) []string
	newFeatureRegistry              func([]Feature) (FeatureRegistry, error)
	featuresAtDSLVersion            func([]Feature, string) ([]Feature, error)
	featuresForWorkflow             func(Definition) ([]Feature, error)
	featuresForGaggle               func(apiv1.GaggleSpec) ([]Feature, error)
	featuresForGoober               func(apiv1.GooberSpec) ([]Feature, error)
	checkFeatureSupport             func([]Feature, bool) []FeatureDiagnostic
	checkWorkflowFeatureSupport     func(Definition, bool) []FeatureDiagnostic
	taskInvocationInputs            func(*Machine, apiv1.Task) map[string]string
	taskLimits                      func(apiv1.Task) apiv1.Limits
	gateLimits                      func(apiv1.Gate) apiv1.Limits
}

// preV30SurfaceProblems is the checkRunsOnPlacement arm for every interpreter
// BEFORE 3.0: the runsOn/repoFrom/commitsRepo surface does not exist in those
// versions, and the frozen packages must not learn it (PO-D0: 2.0 never
// learns distributed features), so the refusal lives here in the router. A
// document that touches none of the fields — every config that exists today —
// produces no problems, keeping the frozen interpreters byte-identical.
//
// The gaggle half is the dsl-3.0.md open point 2 compile-time statement: a
// gaggle that declares runsOn (or reaches this router while 3.0 is the newest
// supported resolution) pairs only with 3.0-pinned workflows — a 2.0-pinned
// workflow in such a gaggle is refused here, never silently stripped of the
// gaggle floor.
func preV30SurfaceProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	version := def.DSLVersion
	if version == "" {
		version = supportmatrix.CurrentDSLVersion
	}
	for _, task := range def.Spec.Tasks {
		if task.RunsOn != nil {
			problems = append(problems, fmt.Sprintf(
				"task %q declares runsOn, which requires dslVersion %q (this workflow pins %q); migrate with `goobers fix --to %s`",
				task.Name, supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
		}
		if task.RepoFrom != nil {
			problems = append(problems, fmt.Sprintf(
				"task %q declares repoFrom, which requires dslVersion %q (this workflow pins %q); migrate with `goobers fix --to %s`",
				task.Name, supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
		}
		if task.CommitsRepo {
			problems = append(problems, fmt.Sprintf(
				"task %q declares commitsRepo, which requires dslVersion %q (this workflow pins %q); migrate with `goobers fix --to %s`",
				task.Name, supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
		}
	}
	if gaggleRunsOn != nil {
		problems = append(problems, fmt.Sprintf(
			"the gaggle declares runsOn, which requires every workflow in the gaggle to pin dslVersion %q (this workflow pins %q); migrate the workflow with `goobers fix --to %s`, or keep the gaggle on requiredCapabilities until then",
			supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
	}
	return problems
}

func noRunsOnProblems(Definition, *apiv1.GaggleRunsOn) []string { return nil }

func noRepoHandoffProblems(Definition) []string { return nil }

var currentInterpreter = versionedInterpreter{
	compile:                         compileCurrent,
	checkWarnings:                   vcurrent.CheckWarnings,
	checkReachability:               vcurrent.CheckReachability,
	checkSchedules:                  vcurrent.CheckSchedules,
	checkTriggerFields:              vcurrent.CheckTriggerFields,
	checkWorkflowAdmission:          vcurrent.CheckWorkflowAdmission,
	checkPushBoundaries:             vcurrent.CheckPushBoundaries,
	checkRunsOnOSTokens:             noRunsOnProblems,
	checkRunsOnRestrictions:         noRunsOnProblems,
	checkRunsOnPlacement:            preV30SurfaceProblems,
	checkRepoHandoffs:               noRepoHandoffProblems,
	checkGateParameters:             vcurrent.CheckGateParameters,
	checkGateOutcomes:               vcurrent.CheckGateOutcomes,
	checkStageRequiredInputs:        vcurrent.CheckStageRequiredInputs,
	checkStageContracts:             vcurrent.CheckStageContracts,
	checkStageContractWarnings:      vcurrent.CheckStageContractWarnings,
	checkStageTimeoutCoherence:      vcurrent.CheckStageTimeoutCoherence,
	checkSubprocessTimeoutCoherence: vcurrent.CheckSubprocessTimeoutCoherence,
	checkPathSimulation:             vcurrent.CheckPathSimulation,
	newFeatureRegistry:              newCurrentFeatureRegistry,
	featuresAtDSLVersion:            vcurrent.FeaturesAtDSLVersion,
	featuresForWorkflow:             vcurrent.FeaturesForWorkflow,
	featuresForGaggle:               vcurrent.FeaturesForGaggle,
	featuresForGoober:               vcurrent.FeaturesForGoober,
	checkFeatureSupport:             vcurrent.CheckFeatureSupport,
	checkWorkflowFeatureSupport:     vcurrent.CheckWorkflowFeatureSupport,
	taskInvocationInputs:            vcurrent.TaskInvocationInputs,
	taskLimits:                      vcurrent.TaskLimits,
	gateLimits:                      vcurrent.GateLimits,
}

var nextInterpreter = versionedInterpreter{
	compile:                         compileNext,
	checkWarnings:                   vnext.CheckWarnings,
	checkReachability:               vnext.CheckReachability,
	checkSchedules:                  vnext.CheckSchedules,
	checkTriggerFields:              vnext.CheckTriggerFields,
	checkWorkflowAdmission:          vnext.CheckWorkflowAdmission,
	checkPushBoundaries:             vnext.CheckPushBoundaries,
	checkRunsOnOSTokens:             noRunsOnProblems,
	checkRunsOnRestrictions:         noRunsOnProblems,
	checkRunsOnPlacement:            preV30SurfaceProblems,
	checkRepoHandoffs:               noRepoHandoffProblems,
	checkGateParameters:             vnext.CheckGateParameters,
	checkGateOutcomes:               vnext.CheckGateOutcomes,
	checkStageRequiredInputs:        vnext.CheckStageRequiredInputs,
	checkStageContracts:             vnext.CheckStageContracts,
	checkStageContractWarnings:      vnext.CheckStageContractWarnings,
	checkStageTimeoutCoherence:      vnext.CheckStageTimeoutCoherence,
	checkSubprocessTimeoutCoherence: vnext.CheckSubprocessTimeoutCoherence,
	checkPathSimulation:             vnext.CheckPathSimulation,
	newFeatureRegistry:              newNextFeatureRegistry,
	featuresAtDSLVersion:            nextFeaturesAtDSLVersion,
	featuresForWorkflow:             featuresForNextWorkflow,
	featuresForGaggle:               featuresForNextGaggle,
	featuresForGoober:               featuresForNextGoober,
	checkFeatureSupport:             checkNextFeatureSupport,
	checkWorkflowFeatureSupport:     checkNextWorkflowFeatureSupport,
	taskInvocationInputs:            vnext.TaskInvocationInputs,
	taskLimits:                      vnext.TaskLimits,
	gateLimits:                      vnext.GateLimits,
}

// v30Interpreter is the DSL 3.0 arm (dsl-3.0.md §8, issue #3505): the
// runsOn/repoFrom surface with its own copy-forward interpreter package.
var v30Interpreter = versionedInterpreter{
	compile:                         compileV30,
	checkWarnings:                   v30.CheckWarnings,
	checkReachability:               v30.CheckReachability,
	checkSchedules:                  v30.CheckSchedules,
	checkTriggerFields:              v30.CheckTriggerFields,
	checkWorkflowAdmission:          v30.CheckWorkflowAdmission,
	checkPushBoundaries:             v30.CheckPushBoundaries,
	checkRunsOnOSTokens:             v30.CheckRunsOnOSTokens,
	checkRunsOnRestrictions:         v30.CheckRunsOnRestrictions,
	checkRunsOnPlacement:            v30.CheckRunsOnPlacement,
	checkRepoHandoffs:               v30.CheckRepoHandoffs,
	checkGateParameters:             v30.CheckGateParameters,
	checkGateOutcomes:               v30.CheckGateOutcomes,
	checkStageRequiredInputs:        v30.CheckStageRequiredInputs,
	checkStageContracts:             v30.CheckStageContracts,
	checkStageContractWarnings:      v30.CheckStageContractWarnings,
	checkStageTimeoutCoherence:      v30.CheckStageTimeoutCoherence,
	checkSubprocessTimeoutCoherence: v30.CheckSubprocessTimeoutCoherence,
	checkPathSimulation:             v30.CheckPathSimulation,
	newFeatureRegistry:              newV30FeatureRegistry,
	featuresAtDSLVersion:            v30FeaturesAtDSLVersion,
	featuresForWorkflow:             featuresForV30Workflow,
	featuresForGaggle:               featuresForV30Gaggle,
	featuresForGoober:               featuresForV30Goober,
	checkFeatureSupport:             checkV30FeatureSupport,
	checkWorkflowFeatureSupport:     checkV30WorkflowFeatureSupport,
	taskInvocationInputs:            v30.TaskInvocationInputs,
	taskLimits:                      v30.TaskLimits,
	gateLimits:                      v30.GateLimits,
}

type compileConfig struct {
	goobers                       map[string]apiv1.GooberSpec
	goobersSet                    bool
	knownChecks                   []string
	knownChecksSet                bool
	knownHarnesses                []string
	knownHarnessesSet             bool
	allowPreviewFeatures          bool
	previewFeaturesSet            bool
	gaggleRequiredCapabilities    []string
	gaggleRequiredCapabilitiesSet bool
	gaggleRunsOn                  *apiv1.GaggleRunsOn
	gaggleRunsOnSet               bool
}

// Option customizes compilation.
type Option func(*compileConfig)

// WithGoobers supplies goober definitions for capability admission.
func WithGoobers(goobers map[string]apiv1.GooberSpec) Option {
	return func(config *compileConfig) {
		config.goobers = goobers
		config.goobersSet = true
	}
}

// WithKnownChecks supplies the registered automated-check names.
func WithKnownChecks(names []string) Option {
	return func(config *compileConfig) {
		config.knownChecks = names
		config.knownChecksSet = true
	}
}

// WithKnownHarnesses supplies the registered agent harness names.
func WithKnownHarnesses(names []string) Option {
	return func(config *compileConfig) {
		config.knownHarnesses = names
		config.knownHarnessesSet = true
	}
}

// WithGaggleRequiredCapabilities supplies the workflow's gaggle-level runner
// capability requirements (GaggleSpec.RequiredCapabilities) so push-boundary
// admission (#2861) evaluates each stage's effective requirement set —
// gaggle-level tokens union stage-level ones.
func WithGaggleRequiredCapabilities(caps []string) Option {
	return func(config *compileConfig) {
		config.gaggleRequiredCapabilities = caps
		config.gaggleRequiredCapabilitiesSet = true
	}
}

// WithGaggleRunsOn supplies the workflow's gaggle-level placement floor
// (GaggleSpec.RunsOn, DSL 3.0) so compilation evaluates each stage's
// effective runsOn — capabilities/restrictions union, OS conflicts error.
// On a pre-3.0 workflow a non-nil floor is itself a compile error: gaggle
// runsOn pairs only with 3.0-pinned workflows (dsl-3.0.md open point 2).
func WithGaggleRunsOn(runsOn *apiv1.GaggleRunsOn) Option {
	return func(config *compileConfig) {
		config.gaggleRunsOn = runsOn
		config.gaggleRunsOnSet = true
	}
}

// PreviewFeaturesAnnotation enables preview DSL features on an instance.
const PreviewFeaturesAnnotation = "goobers.dev/allow-preview-features"

// PreviewFeaturesEnabled reports whether annotations explicitly enable previews.
func PreviewFeaturesEnabled(annotations map[string]string) bool {
	return annotations[PreviewFeaturesAnnotation] == "true"
}

// WithPreviewFeatures applies preview-feature acknowledgement to compilation.
func WithPreviewFeatures(enabled bool) Option {
	return func(config *compileConfig) {
		config.allowPreviewFeatures = enabled
		config.previewFeaturesSet = true
	}
}

// Compile dispatches a pinned definition to its versioned interpreter.
func Compile(def Definition, opts ...Option) (*Machine, error) {
	def.Spec.Tasks = append([]apiv1.Task(nil), def.Spec.Tasks...)
	for i := range def.Spec.Tasks {
		if def.Spec.Tasks[i].OutboxMirrorPath == "" {
			def.Spec.Tasks[i].OutboxMirrorPath = def.Spec.OutboxMirrorPath
		}
	}
	if err := runcontrol.ValidateWorkflow(def.Spec); err != nil {
		return nil, fmt.Errorf("compile workflow %q: %w", def.Name, err)
	}
	interpreter, err := interpreterForVersion(def.DSLVersion)
	if err != nil {
		return nil, fmt.Errorf("compile workflow %q: %w", def.Name, err)
	}
	config := compileConfig{}
	for _, opt := range opts {
		opt(&config)
	}
	return interpreter.compile(def, config)
}

// refusePreV30Surface fails a pre-3.0 compile that touches the 3.0-only
// surface. It lives in the router because the older interpreters are frozen
// and must never learn the fields (PO-D0); the same refusal reaches `goobers
// validate` through the checkRunsOnPlacement arm.
func refusePreV30Surface(def Definition, config compileConfig) error {
	problems := preV30SurfaceProblems(def, config.gaggleRunsOn)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
}

func compileCurrent(def Definition, config compileConfig) (*Machine, error) {
	if err := refusePreV30Surface(def, config); err != nil {
		return nil, err
	}
	var opts []vcurrent.Option
	if config.goobersSet {
		opts = append(opts, vcurrent.WithGoobers(goobersForCapabilityAdmission(config.goobers)))
	}
	if config.knownChecksSet {
		opts = append(opts, vcurrent.WithKnownChecks(config.knownChecks))
	}
	if config.knownHarnessesSet {
		opts = append(opts, vcurrent.WithKnownHarnesses(config.knownHarnesses))
	}
	if config.previewFeaturesSet {
		opts = append(opts, vcurrent.WithPreviewFeatures(config.allowPreviewFeatures))
	}
	if config.gaggleRequiredCapabilitiesSet {
		opts = append(opts, vcurrent.WithGaggleRequiredCapabilities(config.gaggleRequiredCapabilities))
	}
	return vcurrent.Compile(def, opts...)
}

func compileNext(def Definition, config compileConfig) (*Machine, error) {
	if err := refusePreV30Surface(def, config); err != nil {
		return nil, err
	}
	var opts []vnext.Option
	if config.goobersSet {
		opts = append(opts, vnext.WithGoobers(goobersForCapabilityAdmission(config.goobers)))
	}
	if config.knownChecksSet {
		opts = append(opts, vnext.WithKnownChecks(config.knownChecks))
	}
	if config.knownHarnessesSet {
		opts = append(opts, vnext.WithKnownHarnesses(config.knownHarnesses))
	}
	if config.previewFeaturesSet {
		opts = append(opts, vnext.WithPreviewFeatures(config.allowPreviewFeatures))
	}
	if config.gaggleRequiredCapabilitiesSet {
		opts = append(opts, vnext.WithGaggleRequiredCapabilities(config.gaggleRequiredCapabilities))
	}
	return vnext.Compile(def, opts...)
}

func compileV30(def Definition, config compileConfig) (*Machine, error) {
	// The inverse surface rule: a 3.0 workflow whose gaggle still declares
	// requiredCapabilities is refused inside the interpreter (its
	// FeaturesForGaggle), so only the runsOn floor is routed through here.
	var opts []v30.Option
	if config.goobersSet {
		opts = append(opts, v30.WithGoobers(goobersForCapabilityAdmission(config.goobers)))
	}
	if config.knownChecksSet {
		opts = append(opts, v30.WithKnownChecks(config.knownChecks))
	}
	if config.knownHarnessesSet {
		opts = append(opts, v30.WithKnownHarnesses(config.knownHarnesses))
	}
	if config.previewFeaturesSet {
		opts = append(opts, v30.WithPreviewFeatures(config.allowPreviewFeatures))
	}
	if config.gaggleRunsOnSet {
		opts = append(opts, v30.WithGaggleRunsOn(config.gaggleRunsOn))
	}
	return v30.Compile(def, opts...)
}

func interpreterForDefinition(def Definition) (*versionedInterpreter, error) {
	return interpreterForVersion(def.DSLVersion)
}

func interpreterForMachine(machine *Machine) (*versionedInterpreter, error) {
	if machine == nil {
		return nil, fmt.Errorf("workflow machine is nil")
	}
	return interpreterForVersion(machine.Def.DSLVersion)
}

func interpreterForVersion(version string) (*versionedInterpreter, error) {
	if version == "" {
		version = supportmatrix.CurrentDSLVersion
	}

	support, ok := supportmatrix.GetDSL().Lookup(version)
	if !ok {
		return nil, fmt.Errorf("DSL version %q is not supported by this build", version)
	}
	if support.Level == supportmatrix.LevelUnsupported {
		if support.Replacement != "" {
			return nil, fmt.Errorf("DSL version %q is unsupported; migrate to %q", version, support.Replacement)
		}
		return nil, fmt.Errorf("DSL version %q is unsupported", version)
	}

	switch version {
	case vcurrent.DSLVersion:
		return &currentInterpreter, nil
	case vnext.DSLVersion:
		return &nextInterpreter, nil
	case v30.DSLVersion:
		return &v30Interpreter, nil
	default:
		return nil, fmt.Errorf("DSL version %q is declared %s but has no interpreter", version, support.Level)
	}
}
