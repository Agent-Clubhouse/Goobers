package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

type versionedInterpreter struct {
	compile                     func(Definition, ...Option) (*Machine, error)
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
	compile:                     vcurrent.Compile,
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

// Option customizes compilation.
type Option = vcurrent.Option

// WithGoobers supplies goober definitions for capability admission.
func WithGoobers(goobers map[string]apiv1.GooberSpec) Option {
	return vcurrent.WithGoobers(goobers)
}

// WithKnownChecks supplies the registered automated-check names.
func WithKnownChecks(names []string) Option {
	return vcurrent.WithKnownChecks(names)
}

// WithKnownHarnesses supplies the registered agent harness names.
func WithKnownHarnesses(names []string) Option {
	return vcurrent.WithKnownHarnesses(names)
}

// PreviewFeaturesAnnotation enables preview DSL features on an instance.
const PreviewFeaturesAnnotation = vcurrent.PreviewFeaturesAnnotation

// PreviewFeaturesEnabled reports whether annotations explicitly enable previews.
func PreviewFeaturesEnabled(annotations map[string]string) bool {
	return vcurrent.PreviewFeaturesEnabled(annotations)
}

// WithPreviewFeatures applies preview-feature acknowledgement to compilation.
func WithPreviewFeatures(enabled bool) Option {
	return vcurrent.WithPreviewFeatures(enabled)
}

// Compile dispatches a pinned definition to its versioned interpreter.
func Compile(def Definition, opts ...Option) (*Machine, error) {
	interpreter, err := interpreterForVersion(def.DSLVersion)
	if err != nil {
		return nil, fmt.Errorf("compile workflow %q: %w", def.Name, err)
	}
	return interpreter.compile(def, opts...)
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
	case supportmatrix.CurrentDSLVersion:
		return &currentInterpreter, nil
	default:
		return nil, fmt.Errorf("DSL version %q is declared %s but has no interpreter", version, support.Level)
	}
}
