package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/supportmatrix"
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

var currentInterpreter = versionedInterpreter{
	compile:                         compileCurrent,
	checkWarnings:                   vcurrent.CheckWarnings,
	checkReachability:               vcurrent.CheckReachability,
	checkSchedules:                  vcurrent.CheckSchedules,
	checkTriggerFields:              vcurrent.CheckTriggerFields,
	checkWorkflowAdmission:          vcurrent.CheckWorkflowAdmission,
	checkPushBoundaries:             vcurrent.CheckPushBoundaries,
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

func compileCurrent(def Definition, config compileConfig) (*Machine, error) {
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
	default:
		return nil, fmt.Errorf("DSL version %q is declared %s but has no interpreter", version, support.Level)
	}
}
