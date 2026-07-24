package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
	vnext "github.com/goobers/goobers/internal/workflow/v_next"
)

type versionedInterpreter struct {
	compile                     func(Definition, compileConfig) (*Machine, error)
	checkWarnings               func(Definition) []string
	checkReachability           func(Definition) []string
	checkSchedules              func(Definition) []string
	checkTriggerFields          func(Definition) []string
	checkWorkflowAdmission      func(Definition, map[string]apiv1.GooberSpec) []string
	checkGateParameters         func(Definition) []string
	checkGateOutcomes           func(Definition) []string
	checkStageRequiredInputs    func(Definition) []string
	checkStageContracts         func(Definition) []string
	checkStageContractWarnings  func(Definition) []string
	checkStageTimeoutCoherence  func(Definition) []string
	featuresForWorkflow         func(Definition) ([]Feature, error)
	checkWorkflowFeatureSupport func(Definition, bool) []FeatureDiagnostic
	taskInvocationInputs        func(*Machine, apiv1.Task) map[string]string
	taskLimits                  func(apiv1.Task) apiv1.Limits
	gateLimits                  func(apiv1.Gate) apiv1.Limits
}

var currentInterpreter = versionedInterpreter{
	compile:                     compileCurrent,
	checkWarnings:               vcurrent.CheckWarnings,
	checkReachability:           vcurrent.CheckReachability,
	checkSchedules:              vcurrent.CheckSchedules,
	checkTriggerFields:          vcurrent.CheckTriggerFields,
	checkWorkflowAdmission:      vcurrent.CheckWorkflowAdmission,
	checkGateParameters:         vcurrent.CheckGateParameters,
	checkGateOutcomes:           vcurrent.CheckGateOutcomes,
	checkStageRequiredInputs:    vcurrent.CheckStageRequiredInputs,
	checkStageContracts:         vcurrent.CheckStageContracts,
	checkStageContractWarnings:  vcurrent.CheckStageContractWarnings,
	checkStageTimeoutCoherence:  vcurrent.CheckStageTimeoutCoherence,
	featuresForWorkflow:         vcurrent.FeaturesForWorkflow,
	checkWorkflowFeatureSupport: vcurrent.CheckWorkflowFeatureSupport,
	taskInvocationInputs:        vcurrent.TaskInvocationInputs,
	taskLimits:                  vcurrent.TaskLimits,
	gateLimits:                  vcurrent.GateLimits,
}

var nextInterpreter = versionedInterpreter{
	compile:                     compileNext,
	checkWarnings:               vnext.CheckWarnings,
	checkReachability:           vnext.CheckReachability,
	checkSchedules:              vnext.CheckSchedules,
	checkTriggerFields:          vnext.CheckTriggerFields,
	checkWorkflowAdmission:      vnext.CheckWorkflowAdmission,
	checkGateParameters:         vnext.CheckGateParameters,
	checkGateOutcomes:           vnext.CheckGateOutcomes,
	checkStageRequiredInputs:    vnext.CheckStageRequiredInputs,
	checkStageContracts:         vnext.CheckStageContracts,
	checkStageContractWarnings:  vnext.CheckStageContractWarnings,
	checkStageTimeoutCoherence:  vnext.CheckStageTimeoutCoherence,
	featuresForWorkflow:         featuresForNextWorkflow,
	checkWorkflowFeatureSupport: checkNextWorkflowFeatureSupport,
	taskInvocationInputs:        vnext.TaskInvocationInputs,
	taskLimits:                  vnext.TaskLimits,
	gateLimits:                  vnext.GateLimits,
}

type compileConfig struct {
	goobers              map[string]apiv1.GooberSpec
	goobersSet           bool
	knownChecks          []string
	knownChecksSet       bool
	knownHarnesses       []string
	knownHarnessesSet    bool
	allowPreviewFeatures bool
	previewFeaturesSet   bool
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
		opts = append(opts, vcurrent.WithGoobers(config.goobers))
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
	return vcurrent.Compile(def, opts...)
}

func compileNext(def Definition, config compileConfig) (*Machine, error) {
	var opts []vnext.Option
	if config.goobersSet {
		opts = append(opts, vnext.WithGoobers(config.goobers))
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
