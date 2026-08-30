package workflow

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/supportmatrix"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
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
	stagePlacements                 func(Definition, apiv1.GaggleSpec, map[string]apiv1.GooberSpec) ([]runnersolve.StageRequirement, error)
	checkRepoHandoffs               func(Definition) []string
	checkGateRunsOn                 func(Definition) []string
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
// BEFORE 3.0: the runsOn/repoFrom/commitsRepo surface — on tasks AND on gates
// (decision 001) — does not exist in those versions, and the frozen packages
// must not learn it (PO-D0: 2.0 never learns distributed features), so the
// refusal lives here in the router. A document that touches none of the
// fields — every config that exists today — produces no problems, keeping the
// frozen interpreters byte-identical.
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
	for _, gate := range def.Spec.Gates {
		if gate.RunsOn != nil {
			problems = append(problems, fmt.Sprintf(
				"gate %q declares runsOn, which requires dslVersion %q (this workflow pins %q); migrate with `goobers fix --to %s`",
				gate.Name, supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
		}
	}
	if gaggleRunsOn != nil {
		problems = append(problems, fmt.Sprintf(
			"the gaggle declares runsOn, which requires every workflow in the gaggle to pin dslVersion %q (this workflow pins %q); migrate the workflow with `goobers fix --to %s`, or keep the gaggle on requiredCapabilities until then",
			supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
	}
	return append(problems, preV30WindowsAdminProblems(def, nil)...)
}

// preV30WindowsAdminProblems refuses the one product-interpreted capability
// token (#3619, runnercap.CapabilityWindowsAdmin) in a pre-3.0 document's
// requiredCapabilities — on its tasks and, when the caller supplies it, on
// the gaggle-level GaggleSpec.RequiredCapabilities it unions in.
//
// requiredCapabilities is an exact-match tag set with no OS, no coherence
// rule (windowsAdminProblems is 3.0-only) and no CAP005 Windows-restriction
// check. Left alone, a 2.0 task naming the token would pin to a class whose
// provides.capabilities claims it and the dispatcher would render that pod as
// ContainerAdministrator — placed by the accident of which runners claim the
// token, exactly the shape the 3.0 rule refuses, and a substrate effect the
// frozen interpreter never learned (PO-D0). So the token is refused here in
// the router like runsOn itself: it exists only as 3.0 runsOn.capabilities
// under an effective runsOn.os: windows. Every other token stays an opaque
// tag on 2.0, byte-identical to before.
//
// Two callers, one rule: refusePreV30Surface (compile, with the gaggle set)
// and preV30StagePlacements (the solver input every admission checkpoint and
// the run-start pin read), so a document that bypasses compile — validate's
// checkpoint solve, PinStagePlacements — is refused just the same.
func preV30WindowsAdminProblems(def Definition, gaggleRequiredCapabilities []string) []string {
	version := def.DSLVersion
	if version == "" {
		version = supportmatrix.CurrentDSLVersion
	}
	var problems []string
	for _, task := range def.Spec.Tasks {
		if runnercap.HasWindowsAdmin(task.RequiredCapabilities) {
			problems = append(problems, fmt.Sprintf(
				"task %q declares requiredCapabilities %q, the ContainerAdministrator identity of a Windows stage pod, which exists only as runsOn.capabilities under runsOn.os: windows on dslVersion %q (this workflow pins %q); migrate with `goobers fix --to %s` and declare runsOn (#3619)",
				task.Name, runnercap.CapabilityWindowsAdmin, supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
		}
	}
	if runnercap.HasWindowsAdmin(gaggleRequiredCapabilities) {
		problems = append(problems, fmt.Sprintf(
			"the gaggle declares requiredCapabilities %q, the ContainerAdministrator identity of a Windows stage pod, which exists only as a gaggle runsOn floor (runsOn.os: windows) over dslVersion %q workflows (this workflow pins %q); migrate the workflow with `goobers fix --to %s` and move the gaggle to runsOn (#3619)",
			runnercap.CapabilityWindowsAdmin, supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
	}
	return problems
}

func noRunsOnProblems(Definition, *apiv1.GaggleRunsOn) []string { return nil }

func noRepoHandoffProblems(Definition) []string { return nil }

// noGateRunsOnProblems is the pre-3.0 checkGateRunsOn arm: a gate runsOn on
// a 2.0 document is already refused by preV30SurfaceProblems (the frozen
// interpreter never sees the field), so the gate-only rules have nothing to
// say.
func noGateRunsOnProblems(Definition) []string { return nil }

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
	stagePlacements:                 preV30StagePlacements,
	checkRepoHandoffs:               noRepoHandoffProblems,
	checkGateRunsOn:                 noGateRunsOnProblems,
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
	stagePlacements:                 v30StagePlacements,
	checkRepoHandoffs:               v30.CheckRepoHandoffs,
	checkGateRunsOn:                 v30.CheckGateRunsOn,
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
	// The task half of the windows-admin refusal is already in
	// preV30SurfaceProblems; the gaggle-level requiredCapabilities are a
	// compile option, so their half is added here.
	problems = append(problems, preV30WindowsAdminProblems(Definition{DSLVersion: def.DSLVersion}, config.gaggleRequiredCapabilities)...)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
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
		// DSL 1.4 is dropped (#3507), so a missing pin can no longer default to
		// it. For AUTHOR-FACING documents a missing dslVersion is now a hard
		// error, enforced at the sole lifecycle checkpoint,
		// api/validate.checkWorkflowDSLVersion (the §8.3 cutover) — every
		// config-load path routes through it, so a real document never reaches
		// the router unpinned. This router fallback exists only for a
		// programmatically-constructed Definition with no pin; it resolves to
		// the back-compat contract version (2.0) rather than fabricating an
		// interpreter for a version the build no longer carries.
		version = supportmatrix.NextDSLVersion
	}

	support, ok := supportmatrix.GetDSL().Lookup(version)
	if !ok {
		return nil, fmt.Errorf("DSL version %q is not supported by this build", version)
	}
	if support.Level == supportmatrix.LevelUnsupported {
		if support.Replacement != "" {
			return nil, fmt.Errorf("DSL version %q is unsupported; migrate with `goobers fix --to %s`", version, support.Replacement)
		}
		return nil, fmt.Errorf("DSL version %q is unsupported", version)
	}

	switch version {
	case vnext.DSLVersion:
		return &nextInterpreter, nil
	case v30.DSLVersion:
		return &v30Interpreter, nil
	default:
		return nil, fmt.Errorf("DSL version %q is declared %s but has no interpreter", version, support.Level)
	}
}
