package workflow

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow/internal/model"
	v20 "github.com/goobers/goobers/internal/workflow/v_2_0"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
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
		version = supportmatrix.V1DSLVersion
	}
	if def.Spec.Backprop != nil {
		problems = append(problems, fmt.Sprintf(
			"workflow declares backprop, which requires dslVersion %q (this workflow pins %q); migrate with `goobers fix --to %s`",
			supportmatrix.V3DSLVersion, version, supportmatrix.V3DSLVersion))
	}
	problems = append(problems, preV31ArtifactSurfaceProblems(def, version)...)
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
		version = supportmatrix.V1DSLVersion
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

var v20Interpreter = versionedInterpreter{
	compile:                         compileNext,
	checkWarnings:                   v20.CheckWarnings,
	checkReachability:               v20.CheckReachability,
	checkSchedules:                  v20.CheckSchedules,
	checkTriggerFields:              v20.CheckTriggerFields,
	checkWorkflowAdmission:          v20.CheckWorkflowAdmission,
	checkPushBoundaries:             v20.CheckPushBoundaries,
	checkRunsOnOSTokens:             noRunsOnProblems,
	checkRunsOnRestrictions:         noRunsOnProblems,
	checkRunsOnPlacement:            preV30SurfaceProblems,
	stagePlacements:                 preV30StagePlacements,
	checkRepoHandoffs:               noRepoHandoffProblems,
	checkGateRunsOn:                 noGateRunsOnProblems,
	checkGateParameters:             v20.CheckGateParameters,
	checkGateOutcomes:               v20.CheckGateOutcomes,
	checkStageRequiredInputs:        v20.CheckStageRequiredInputs,
	checkStageContracts:             v20.CheckStageContracts,
	checkStageContractWarnings:      v20.CheckStageContractWarnings,
	checkStageTimeoutCoherence:      v20.CheckStageTimeoutCoherence,
	checkSubprocessTimeoutCoherence: v20.CheckSubprocessTimeoutCoherence,
	checkPathSimulation:             v20.CheckPathSimulation,
	newFeatureRegistry:              newNextFeatureRegistry,
	featuresAtDSLVersion:            nextFeaturesAtDSLVersion,
	featuresForWorkflow:             featuresForNextWorkflow,
	featuresForGaggle:               featuresForNextGaggle,
	featuresForGoober:               featuresForNextGoober,
	checkFeatureSupport:             checkNextFeatureSupport,
	checkWorkflowFeatureSupport:     checkNextWorkflowFeatureSupport,
	taskInvocationInputs:            v20.TaskInvocationInputs,
	taskLimits:                      v20.TaskLimits,
	gateLimits:                      v20.GateLimits,
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
	checkRunsOnPlacement:            v30PlacementWithPreV31Surface,
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

// v31Interpreter carries DSL 3.0 forward and adds only the named-artifact
// contract surface. Runtime lowering/resolution intentionally remains out of
// scope for this version.
var v31Interpreter = versionedInterpreter{
	compile:                         compileV31,
	checkWarnings:                   v30.CheckWarnings,
	checkReachability:               v30.CheckReachability,
	checkSchedules:                  v30.CheckSchedules,
	checkTriggerFields:              v30.CheckTriggerFields,
	checkWorkflowAdmission:          v30.CheckWorkflowAdmission,
	checkPushBoundaries:             v30.CheckPushBoundaries,
	checkRunsOnOSTokens:             v30.CheckRunsOnOSTokens,
	checkRunsOnRestrictions:         v30.CheckRunsOnRestrictions,
	checkRunsOnPlacement:            v31Placement,
	stagePlacements:                 v30StagePlacements,
	checkRepoHandoffs:               v30.CheckRepoHandoffs,
	checkGateRunsOn:                 v30.CheckGateRunsOn,
	checkGateParameters:             v30.CheckGateParameters,
	checkGateOutcomes:               v30.CheckGateOutcomes,
	checkStageRequiredInputs:        v30.CheckStageRequiredInputs,
	checkStageContracts:             v31StageContracts,
	checkStageContractWarnings:      v30.CheckStageContractWarnings,
	checkStageTimeoutCoherence:      v30.CheckStageTimeoutCoherence,
	checkSubprocessTimeoutCoherence: v30.CheckSubprocessTimeoutCoherence,
	checkPathSimulation:             v30.CheckPathSimulation,
	newFeatureRegistry:              newV31FeatureRegistry,
	featuresAtDSLVersion:            v31FeaturesAtDSLVersion,
	featuresForWorkflow:             featuresForV31Workflow,
	featuresForGaggle:               featuresForV31Gaggle,
	featuresForGoober:               featuresForV31Goober,
	checkFeatureSupport:             checkV31FeatureSupport,
	checkWorkflowFeatureSupport:     checkV31WorkflowFeatureSupport,
	taskInvocationInputs:            v30.TaskInvocationInputs,
	taskLimits:                      v30.TaskLimits,
	gateLimits:                      v30.GateLimits,
}

type compileConfig struct {
	goobers                             map[string]apiv1.GooberSpec
	goobersSet                          bool
	knownChecks                         []string
	knownChecksSet                      bool
	knownHarnesses                      []string
	knownHarnessesSet                   bool
	knownExternalTelemetryConnectors    []string
	knownExternalTelemetryConnectorsSet bool
	allowPreviewFeatures                bool
	previewFeaturesSet                  bool
	gaggleRequiredCapabilities          []string
	gaggleRequiredCapabilitiesSet       bool
	gaggleRunsOn                        *apiv1.GaggleRunsOn
	gaggleRunsOnSet                     bool
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

// WithKnownExternalTelemetryConnectors supplies the connector names actually
// configured on this instance (#4341), so an unresolvable connector is
// rejected at compile time with a specific diagnostic instead of only at run
// time, deep inside a pod's executor construction.
func WithKnownExternalTelemetryConnectors(names []string) Option {
	return func(config *compileConfig) {
		config.knownExternalTelemetryConnectors = names
		config.knownExternalTelemetryConnectorsSet = true
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
	if problems := CheckOutbox(def); len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	if def.Spec.Backprop != nil && def.Spec.Backprop.Version != "v1" {
		return nil, fmt.Errorf("invalid workflow %q: backprop.version must be %q", def.Name, "v1")
	}
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
	var opts []v20.Option
	if config.goobersSet {
		opts = append(opts, v20.WithGoobers(goobersForCapabilityAdmission(config.goobers)))
	}
	if config.knownChecksSet {
		opts = append(opts, v20.WithKnownChecks(config.knownChecks))
	}
	if config.knownHarnessesSet {
		opts = append(opts, v20.WithKnownHarnesses(config.knownHarnesses))
	}
	if config.knownExternalTelemetryConnectorsSet {
		opts = append(opts, v20.WithKnownExternalTelemetryConnectors(config.knownExternalTelemetryConnectors))
	}
	if config.previewFeaturesSet {
		opts = append(opts, v20.WithPreviewFeatures(config.allowPreviewFeatures))
	}
	if config.gaggleRequiredCapabilitiesSet {
		opts = append(opts, v20.WithGaggleRequiredCapabilities(config.gaggleRequiredCapabilities))
	}
	return v20.Compile(def, opts...)
}

func compileV30(def Definition, config compileConfig) (*Machine, error) {
	if problems := preV31ArtifactSurfaceProblems(def, supportmatrix.V3DSLVersion); len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	return compileV30Base(def, config)
}

func compileV31(def Definition, config compileConfig) (*Machine, error) {
	if problems := artifactContractProblems(def); len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	machine, err := compileV30Base(def, config)
	if err != nil {
		return nil, err
	}
	bindings, problems := lowerArtifactBindings(machine)
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	machine.SetArtifactBindings(bindings)
	return machine, nil
}

func compileV30Base(def Definition, config compileConfig) (*Machine, error) {
	// Check the legacy gaggle floor before routing: the interpreter receives
	// only runsOn, so dropping requiredCapabilities here would lose constraints.
	if _, err := v30.FeaturesForGaggle(apiv1.GaggleSpec{RequiredCapabilities: config.gaggleRequiredCapabilities}); err != nil {
		return nil, fmt.Errorf("invalid workflow %q: %w", def.Name, err)
	}
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
	if config.knownExternalTelemetryConnectorsSet {
		opts = append(opts, v30.WithKnownExternalTelemetryConnectors(config.knownExternalTelemetryConnectors))
	}
	if config.previewFeaturesSet {
		opts = append(opts, v30.WithPreviewFeatures(config.allowPreviewFeatures))
	}
	if config.gaggleRunsOnSet {
		opts = append(opts, v30.WithGaggleRunsOn(config.gaggleRunsOn))
	}
	return v30.Compile(def, opts...)
}

func v30PlacementWithPreV31Surface(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	problems := v30.CheckRunsOnPlacement(def, gaggleRunsOn)
	return append(problems, preV31ArtifactSurfaceProblems(def, supportmatrix.V3DSLVersion)...)
}

func v31Placement(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	return v30.CheckRunsOnPlacement(def, gaggleRunsOn)
}

func v31StageContracts(def Definition) []string {
	problems := v30.CheckStageContracts(def)
	return append(problems, artifactContractProblems(def)...)
}

func preV31ArtifactSurfaceProblems(def Definition, version string) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.ArtifactSlots != nil {
			problems = append(problems, fmt.Sprintf(
				"task %q declares artifactSlots, which requires dslVersion %q (this workflow pins %q)",
				task.Name, supportmatrix.V31DSLVersion, version))
		}
		if task.ArtifactInputs != nil {
			problems = append(problems, fmt.Sprintf(
				"task %q declares artifactInputs, which requires dslVersion %q (this workflow pins %q)",
				task.Name, supportmatrix.V31DSLVersion, version))
		}
	}
	return problems
}

func artifactContractProblems(def Definition) []string {
	_, problems := collectArtifactContracts(def)
	if len(problems) > 0 {
		return problems
	}
	machine, buildProblems := artifactCheckMachine(def)
	if len(buildProblems) > 0 {
		return buildProblems
	}
	_, problems = lowerArtifactBindings(machine)
	return problems
}

type artifactContractIndex struct {
	slots     map[string]map[string]apiv1.ArtifactSlot
	taskNames map[string]struct{}
}

func collectArtifactContracts(def Definition) (artifactContractIndex, []string) {
	index := artifactContractIndex{
		slots:     make(map[string]map[string]apiv1.ArtifactSlot, len(def.Spec.Tasks)),
		taskNames: make(map[string]struct{}, len(def.Spec.Tasks)),
	}
	taskNames := make(map[string]struct{}, len(def.Spec.Tasks))
	var problems []string
	for _, task := range def.Spec.Tasks {
		taskNames[task.Name] = struct{}{}
		index.taskNames[task.Name] = struct{}{}
		slots := make(map[string]apiv1.ArtifactSlot, len(task.ArtifactSlots))
		for _, slot := range task.ArtifactSlots {
			if !validArtifactContractName(slot.Name) {
				problems = append(problems, fmt.Sprintf("task %q artifactSlots contains invalid slot name %q", task.Name, slot.Name))
				continue
			}
			if _, exists := slots[slot.Name]; exists {
				problems = append(problems, fmt.Sprintf("task %q artifactSlots repeats slot %q", task.Name, slot.Name))
				continue
			}
			slots[slot.Name] = slot
			if strings.TrimSpace(slot.MediaType) != slot.MediaType {
				problems = append(problems, fmt.Sprintf("task %q artifact slot %q has a blank mediaType", task.Name, slot.Name))
			}
			if strings.TrimSpace(slot.SchemaPath) != slot.SchemaPath {
				problems = append(problems, fmt.Sprintf("task %q artifact slot %q schemaPath must not have leading or trailing whitespace", task.Name, slot.Name))
			}
			if slot.MaxSize < 0 {
				problems = append(problems, fmt.Sprintf("task %q artifact slot %q maxSize must be non-negative", task.Name, slot.Name))
			}
		}
		if len(slots) > 0 {
			index.slots[task.Name] = slots
		}
	}
	for _, task := range def.Spec.Tasks {
		for local, ref := range task.ArtifactInputs {
			if !validArtifactContractName(local) {
				problems = append(problems, fmt.Sprintf("task %q artifactInputs contains invalid local input name %q", task.Name, local))
			}
			producer, slot, ok := splitArtifactInputRef(ref.From)
			if !ok {
				problems = append(problems, fmt.Sprintf("task %q artifact input %q must reference a producer slot as producer.slot", task.Name, local))
				continue
			}
			if _, exists := taskNames[producer]; !exists {
				problems = append(problems, fmt.Sprintf("task %q artifact input %q references unknown producer task %q", task.Name, local, producer))
				continue
			}
			producerSlots := index.slots[producer]
			producerSlot, exists := producerSlots[slot]
			if !exists {
				problems = append(problems, fmt.Sprintf("task %q artifact input %q references unknown artifact slot %q on producer %q", task.Name, local, slot, producer))
			}
			problems = append(problems, artifactInputContractProblems(task.Name, local, ref, producer, slot, producerSlot, exists)...)
		}
	}
	sort.Strings(problems)
	return index, problems
}

func artifactInputContractProblems(taskName, local string, ref apiv1.ArtifactInputRef, producer, slot string, producerSlot apiv1.ArtifactSlot, slotExists bool) []string {
	var problems []string
	if strings.TrimSpace(ref.MediaType) != ref.MediaType {
		problems = append(problems, fmt.Sprintf("task %q artifact input %q has a blank mediaType", taskName, local))
	}
	if strings.TrimSpace(ref.SchemaPath) != ref.SchemaPath {
		problems = append(problems, fmt.Sprintf("task %q artifact input %q schemaPath must not have leading or trailing whitespace", taskName, local))
	}
	if !slotExists {
		return problems
	}
	if ref.MediaType != "" {
		if producerSlot.MediaType == "" {
			problems = append(problems, fmt.Sprintf(
				"task %q artifact input %q expects mediaType %q, but producer %q slot %q declares no mediaType",
				taskName, local, ref.MediaType, producer, slot))
		} else if ref.MediaType != producerSlot.MediaType {
			problems = append(problems, fmt.Sprintf(
				"task %q artifact input %q expects mediaType %q, but producer %q slot %q declares %q",
				taskName, local, ref.MediaType, producer, slot, producerSlot.MediaType))
		}
	}
	if ref.SchemaPath != "" {
		if producerSlot.SchemaPath == "" {
			problems = append(problems, fmt.Sprintf(
				"task %q artifact input %q expects schemaPath %q, but producer %q slot %q declares no schemaPath",
				taskName, local, ref.SchemaPath, producer, slot))
		} else if ref.SchemaPath != producerSlot.SchemaPath {
			problems = append(problems, fmt.Sprintf(
				"task %q artifact input %q expects schemaPath %q, but producer %q slot %q declares %q",
				taskName, local, ref.SchemaPath, producer, slot, producerSlot.SchemaPath))
		}
	}
	return problems
}

func artifactCheckMachine(def Definition) (*Machine, []string) {
	tasks := make(map[string]apiv1.Task, len(def.Spec.Tasks))
	gates := make(map[string]apiv1.Gate, len(def.Spec.Gates))
	parallels := make(map[string]apiv1.Parallel, len(def.Spec.Parallels))
	for _, task := range def.Spec.Tasks {
		tasks[task.Name] = task
	}
	for _, gate := range def.Spec.Gates {
		gates[gate.Name] = gate
	}
	for _, parallel := range def.Spec.Parallels {
		parallels[parallel.Name] = parallel
	}
	machine, err := model.NewMachine(def, tasks, gates, parallels, model.Graph{Start: def.Spec.Start})
	if err != nil {
		return nil, []string{fmt.Sprintf("digest workflow %q: %v", def.Name, err)}
	}
	return machine, nil
}

func lowerArtifactBindings(machine *Machine) (map[string]map[string]model.ArtifactBinding, []string) {
	index, problems := collectArtifactContracts(machine.Def)
	if len(problems) > 0 {
		return nil, problems
	}
	bindings := make(map[string]map[string]model.ArtifactBinding)
	for _, task := range machine.Def.Spec.Tasks {
		for _, local := range sortedArtifactInputNames(task.ArtifactInputs) {
			ref := task.ArtifactInputs[local]
			producer, slot, ok := splitArtifactInputRef(ref.From)
			if !ok {
				continue
			}
			producerSlot, exists := index.slots[producer][slot]
			if !exists {
				continue
			}
			if producer == task.Name {
				problems = append(problems, fmt.Sprintf(
					"task %q artifact input %q references itself; artifactInputs must name an upstream producer",
					task.Name, local))
				continue
			}
			if !artifactProducerDominatesConsumer(machine, producer, task.Name) {
				problems = append(problems, fmt.Sprintf(
					"task %q artifact input %q references producer %q, but %q does not run on every successful path before %q",
					task.Name, local, producer, producer, task.Name))
				continue
			}
			if bindings[task.Name] == nil {
				bindings[task.Name] = make(map[string]model.ArtifactBinding)
			}
			bindings[task.Name][local] = model.ArtifactBinding{
				ConsumerTask: task.Name,
				LocalName:    local,
				ProducerTask: producer,
				SlotName:     slot,
				MediaType:    producerSlot.MediaType,
				SchemaPath:   producerSlot.SchemaPath,
				MaxSize:      producerSlot.MaxSize,
			}
		}
	}
	sort.Strings(problems)
	return bindings, problems
}

func sortedArtifactInputNames(inputs map[string]apiv1.ArtifactInputRef) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func artifactProducerDominatesConsumer(machine *Machine, producer, consumer string) bool {
	owner := artifactBranchOwnership(machine)
	if ref, inBranch := owner[producer]; inBranch {
		if consumerRef, consumerInBranch := owner[consumer]; consumerInBranch &&
			consumerRef.parallel == ref.parallel && consumerRef.branch == ref.branch {
			return artifactPrecedesWithinBranchOnEveryPath(machine, ref.start, producer, consumer)
		}
		parallel, ok := machine.Parallel(ref.parallel)
		if !ok || !artifactPrecedesBranchJoinOnEveryPath(machine, ref.start, producer) {
			return false
		}
		return consumer == parallel.Join || artifactPrecedesOnEveryPath(machine, parallel.Join, consumer)
	}
	return artifactPrecedesOnEveryPath(machine, producer, consumer)
}

type artifactBranchRef struct {
	parallel string
	branch   string
	start    string
}

func artifactBranchOwnership(machine *Machine) map[string]artifactBranchRef {
	owner := make(map[string]artifactBranchRef)
	for _, parallel := range machine.Def.Spec.Parallels {
		for _, branch := range parallel.Branches {
			ref := artifactBranchRef{parallel: parallel.Name, branch: branch.Name, start: branch.Start}
			for _, state := range artifactBranchBody(machine, branch.Start) {
				if _, exists := owner[state]; !exists {
					owner[state] = ref
				}
			}
		}
	}
	return owner
}

func artifactPrecedesWithinBranchOnEveryPath(machine *Machine, start, producer, consumer string) bool {
	if producer == consumer || start == producer {
		return true
	}
	reachable := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == producer || state == TargetJoin || state == TerminalComplete || model.IsReservedTarget(state) || reachable[state] {
			continue
		}
		if !machine.Has(state) {
			continue
		}
		reachable[state] = true
		stack = append(stack, machine.Outgoing(state)...)
	}
	return !reachable[consumer]
}

func artifactBranchBody(machine *Machine, start string) []string {
	seen := map[string]bool{}
	stack := []string{start}
	var states []string
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == TerminalComplete || model.IsReservedAnyTarget(state) || seen[state] || !machine.Has(state) {
			continue
		}
		seen[state] = true
		states = append(states, state)
		stack = append(stack, machine.Outgoing(state)...)
	}
	sort.Strings(states)
	return states
}

func artifactPrecedesBranchJoinOnEveryPath(machine *Machine, start, producer string) bool {
	if start == producer {
		return true
	}
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == producer || seen[state] {
			continue
		}
		if state == TargetJoin {
			return false
		}
		if state == TerminalComplete || model.IsReservedAnyTarget(state) || !machine.Has(state) {
			continue
		}
		seen[state] = true
		stack = append(stack, machine.Outgoing(state)...)
	}
	return true
}

func artifactPrecedesOnEveryPath(machine *Machine, producer, consumer string) bool {
	if producer == consumer {
		return true
	}
	if machine.Def.Spec.Start == producer {
		return true
	}
	reachable := map[string]bool{}
	stack := []string{machine.Def.Spec.Start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == producer || state == TerminalComplete || model.IsReservedTarget(state) || reachable[state] {
			continue
		}
		if !machine.Has(state) {
			continue
		}
		reachable[state] = true
		stack = append(stack, artifactOutgoing(machine, state)...)
	}
	return !reachable[consumer]
}

func artifactOutgoing(machine *Machine, state string) []string {
	joinTargets := artifactJoinTargets(machine)
	if parallel, ok := machine.Parallel(state); ok {
		targets := make([]string, 0, len(parallel.Branches)+1)
		for _, branch := range parallel.Branches {
			targets = append(targets, branch.Start)
		}
		if parallel.OnFailure != "" {
			targets = append(targets, parallel.OnFailure)
		}
		return targets
	}
	out := append([]string(nil), machine.Outgoing(state)...)
	for i, target := range out {
		if target == TargetJoin {
			if join, ok := joinTargets[state]; ok {
				out[i] = join
			}
		}
	}
	return out
}

func artifactJoinTargets(machine *Machine) map[string]string {
	targets := make(map[string]string)
	for _, parallel := range machine.Def.Spec.Parallels {
		for _, branch := range parallel.Branches {
			for _, terminal := range artifactJoinTerminalStates(machine, branch.Start) {
				targets[terminal] = parallel.Join
			}
		}
	}
	return targets
}

func artifactJoinTerminalStates(machine *Machine, start string) []string {
	var terminals []string
	for _, state := range artifactBranchBody(machine, start) {
		for _, target := range machine.Outgoing(state) {
			if target == TargetJoin {
				terminals = append(terminals, state)
				break
			}
		}
	}
	sort.Strings(terminals)
	return terminals
}

func splitArtifactInputRef(ref string) (producer, slot string, ok bool) {
	dot := strings.IndexByte(ref, '.')
	if dot <= 0 || dot != strings.LastIndexByte(ref, '.') || dot == len(ref)-1 {
		return "", "", false
	}
	producer, slot = ref[:dot], ref[dot+1:]
	return producer, slot, producer != "" && !strings.Contains(producer, ".") && validArtifactContractName(slot)
}

func validArtifactContractName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
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
		version = supportmatrix.V2DSLVersion
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
	case v20.DSLVersion:
		return &v20Interpreter, nil
	case v30.DSLVersion:
		return &v30Interpreter, nil
	case supportmatrix.V31DSLVersion:
		return &v31Interpreter, nil
	default:
		return nil, fmt.Errorf("DSL version %q is declared %s but has no interpreter", version, support.Level)
	}
}
