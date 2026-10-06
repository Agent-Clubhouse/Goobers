// Package childworkflow validates agent-authored proposals against a pinned
// parent grant. Validation is advisory: it neither grants credentials nor starts
// a run. Admission must repeat validation and enforce the grant at dispatch.
package childworkflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	yaml "gopkg.in/yaml.v3"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/strictyaml"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

const (
	// MaxProposalBytes bounds one submitted Workflow before YAML parsing.
	MaxProposalBytes = 1 << 20
	// MaxProposalStates bounds the total task, gate, and parallel state count.
	MaxProposalStates = 128
)

// Backend names an already selected execution substrate, not a proposal choice.
type Backend string

const (
	// BackendRunner selects the local runner's executable placement surface.
	BackendRunner Backend = "runner"
	// BackendEngine selects engine placement and its supported-feature checks.
	BackendEngine Backend = "engine"
)

// AdmissionContext is supplied by the trusted caller from the parent's pinned
// catalog, never from proposal metadata or agent-provided policy. Goobers must
// already have passed normal harness/model/MCP configuration admission.
// GrantedCapabilities and AllowPRPublication are the enclosing effective grant;
// the parent's childWorkflows allowlists can narrow, but cannot widen, it.
type AdmissionContext struct {
	Config                           *instance.Config
	Gaggle                           apiv1.Gaggle
	ParentTask                       apiv1.Task
	ConfigDigest                     string
	GrantedCapabilities              []string
	AllowPRPublication               bool
	Goobers                          map[string]apiv1.GooberSpec
	KnownChecks                      []string
	KnownHarnesses                   []string
	KnownExternalTelemetryConnectors []string
	AllowPreviewFeatures             bool
	Backend                          Backend
}

// Diagnostic identifies a refused boundary without returning proposal source.
type Diagnostic struct {
	Code    string `json:"code"`
	Stage   string `json:"stage,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ValidationError contains the typed reasons a proposal was refused.
type ValidationError struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func (e *ValidationError) Error() string {
	if len(e.Diagnostics) == 0 {
		return "invalid child workflow proposal"
	}
	return fmt.Sprintf("child workflow %s: %s", e.Diagnostics[0].Code, e.Diagnostics[0].Message)
}

// Proposal is a validated, independent snapshot. It is not an admission receipt.
// SourceDigest preserves exact submitted bytes; CanonicalDigest ignores YAML
// formatting, and Machine.Digest is the normal compiler's execution identity.
type Proposal struct {
	Source          []byte
	SourceDigest    string
	CanonicalDigest string
	ConfigDigest    string
	PolicyDigest    string
	Workflow        apiv1.Workflow
	Machine         *workflow.Machine
	Placements      []engine.PinnedPlacement
}

// Validator owns a deep copy of its trusted inputs. Caller mutations cannot
// widen the policy or change the catalog between validation attempts.
type Validator struct {
	context AdmissionContext
	schema  *validate.Validator
}

// NewValidator snapshots trusted inputs for side-effect-free proposal checks.
func NewValidator(input AdmissionContext) (*Validator, error) {
	if input.Config == nil || input.Gaggle.Name == "" || input.ConfigDigest == "" {
		return nil, errors.New("child validation requires pinned config, gaggle, and config digest")
	}
	if input.ParentTask.Type != apiv1.TaskAgentic || input.ParentTask.ChildWorkflows == nil || input.ParentTask.Name == "" {
		return nil, errors.New("child validation requires an opted-in pinned parent agentic task")
	}
	if input.Backend != BackendRunner && input.Backend != BackendEngine {
		return nil, errors.New("child validation requires an explicit execution backend")
	}
	policy := input.ParentTask.ChildWorkflows
	if len(policy.AllowedGoobers) == 0 || len(policy.AllowedGoobers) > 128 || policy.EffectiveMaxChildren() < 1 || policy.EffectiveMaxChildren() > apiv1.MaxChildWorkflows {
		return nil, errors.New("invalid pinned child-workflow policy")
	}
	for _, name := range policy.AllowedGoobers {
		goober, ok := input.Goobers[name]
		if !ok || (goober.Gaggle != "" && goober.Gaggle != input.Gaggle.Name) {
			return nil, fmt.Errorf("pinned child policy names an unavailable Goober %q", name)
		}
	}
	for _, grant := range append(slices.Clone(policy.AllowedCapabilities), input.GrantedCapabilities...) {
		if !capability.Known(grant) || !capability.StageDeclarable(grant) {
			return nil, fmt.Errorf("pinned child grant names an invalid capability %q", grant)
		}
	}
	// Only serializable declarative config is retained. Private live counters and
	// engine-selection caches are not inputs to the pure placement functions.
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("snapshot child validation context: %w", err)
	}
	var pinned AdmissionContext
	if err := json.Unmarshal(raw, &pinned); err != nil {
		return nil, err
	}
	schema, err := validate.New()
	if err != nil {
		return nil, err
	}
	return &Validator{context: pinned, schema: schema}, nil
}

func refusal(code, stage, field, message string) error {
	return &ValidationError{Diagnostics: []Diagnostic{{Code: code, Stage: stage, Field: field, Message: message}}}
}

// Validate applies the canonical Workflow schema and compiler. A generated
// Workflow declares exactly one plain manual trigger; it must never be added to
// the scheduler catalog. Only the parent's future child-start path may admit it.
func (v *Validator) Validate(source []byte) (*Proposal, error) {
	wf, canonical, err := v.parseProposal(source)
	if err != nil {
		return nil, err
	}
	if err := v.checkScope(wf); err != nil {
		return nil, err
	}
	if err := v.checkAuthority(wf); err != nil {
		return nil, err
	}
	goobers := make(map[string]apiv1.GooberSpec)
	for _, name := range v.context.ParentTask.ChildWorkflows.AllowedGoobers {
		goobers[name] = v.context.Goobers[name]
	}
	machine, err := v.compileProposal(wf, goobers)
	if err != nil {
		return nil, err
	}
	pins, err := v.pinProposal(wf, machine.Def, goobers)
	if err != nil {
		return nil, err
	}
	policyBytes, _ := json.Marshal(struct {
		Policy      *apiv1.ChildWorkflowPolicy
		Grants      []string
		Publication bool
		Goobers     map[string]apiv1.GooberSpec
	}{v.context.ParentTask.ChildWorkflows, v.context.GrantedCapabilities, v.context.AllowPRPublication, v.context.Goobers})
	return &Proposal{Source: slices.Clone(source), SourceDigest: digest(source), CanonicalDigest: digest(canonical),
		ConfigDigest: v.context.ConfigDigest, PolicyDigest: digest(policyBytes), Workflow: wf, Machine: machine, Placements: pins}, nil
}

func (v *Validator) parseProposal(source []byte) (apiv1.Workflow, []byte, error) {
	if len(source) == 0 || len(source) > MaxProposalBytes {
		return apiv1.Workflow{}, nil, refusal("size", "", "", "proposal must contain 1 to 1048576 bytes")
	}
	if err := singleDocument(source); err != nil {
		return apiv1.Workflow{}, nil, err
	}
	raw, err := strictyaml.YAMLToJSON(source)
	if err != nil {
		return apiv1.Workflow{}, nil, refusal("syntax", "", "", "invalid YAML or duplicate mapping key")
	}
	// Inspect control fields before schema validation so recursion has a stable
	// diagnostic even on binaries whose schema does not yet admit the opt-in.
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // identity must not round integers through float64
	if err := decoder.Decode(&object); err != nil {
		return apiv1.Workflow{}, nil, refusal("syntax", "", "", "expected one Workflow object")
	}
	spec, _ := object["spec"].(map[string]any)
	tasks, _ := spec["tasks"].([]any)
	for _, value := range tasks {
		task, _ := value.(map[string]any)
		if _, present := task["childWorkflows"]; present {
			return apiv1.Workflow{}, nil, refusal("recursion", "", "spec.tasks.childWorkflows", "a generated child cannot opt into child workflows")
		}
	}
	if object["dslVersion"] != supportmatrix.V31DSLVersion {
		return apiv1.Workflow{}, nil, refusal("dsl_version", "", "dslVersion", "a child must explicitly pin DSL 3.1")
	}
	triggers, _ := spec["triggers"].([]any)
	if len(triggers) != 1 {
		return apiv1.Workflow{}, nil, refusal("trigger", "", "spec.triggers", "a child requires exactly one plain manual trigger")
	}
	trigger, _ := triggers[0].(map[string]any)
	if len(trigger) != 1 || trigger["type"] != "manual" {
		return apiv1.Workflow{}, nil, refusal("trigger", "", "spec.triggers", "a child requires exactly one manual trigger with no other fields")
	}
	if err := v.schema.ValidateJSON("workflow.schema.json", raw); err != nil {
		// Schema errors may quote arbitrary authored values. Return the boundary
		// here; the author retains their source for normal local schema tooling.
		field := ""
		var problem *jsonschema.ValidationError
		if errors.As(err, &problem) {
			for len(problem.Causes) > 0 {
				problem = problem.Causes[0]
			}
			field = problem.InstanceLocation
		}
		return apiv1.Workflow{}, nil, refusal("schema", "", field, "proposal does not satisfy the canonical Workflow schema")
	}
	var wf apiv1.Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return apiv1.Workflow{}, nil, refusal("schema", "", "", "cannot decode Workflow")
	}
	canonical, _ := json.Marshal(object) // map keys sorted; authored omissions preserved
	return wf, canonical, nil
}

func (v *Validator) checkScope(wf apiv1.Workflow) error {
	if len(wf.Spec.Tasks)+len(wf.Spec.Gates)+len(wf.Spec.Parallels) > MaxProposalStates {
		return refusal("states", "", "spec", "a child may declare at most 128 states")
	}
	if wf.Spec.Gaggle != v.context.Gaggle.Name || (wf.Namespace != "" && wf.Namespace != v.context.Gaggle.Namespace) {
		return refusal("scope", "", "spec.gaggle", "child must belong to the pinned parent gaggle and namespace")
	}
	if wf.Spec.OutboxMirrorPath != "" || wf.Spec.TutorScope != nil {
		return refusal("scope", "", "spec", "child cannot choose a host outbox path or configuration-repository tutor scope")
	}
	if workflow.PreviewFeaturesEnabled(wf.Annotations) && !v.context.AllowPreviewFeatures {
		return refusal("preview", "", "metadata.annotations", "proposal cannot enable preview features beyond the pinned policy")
	}
	return nil
}

func (v *Validator) compileProposal(wf apiv1.Workflow, goobers map[string]apiv1.GooberSpec) (*workflow.Machine, error) {
	def := workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations}
	if problems := workflow.CheckProviderStageInputs(def); len(problems) != 0 {
		return nil, refusal("provider_inputs", "", "spec.tasks.inputs", bounded(strings.Join(problems, "; ")))
	}
	// Non-nil empty catalogs are significant: a nil registry disables some
	// normal compiler admission checks. Supply every catalog explicitly.
	machine, err := workflow.Compile(def,
		workflow.WithGoobers(goobers),
		workflow.WithKnownChecks(append([]string{}, v.context.KnownChecks...)),
		workflow.WithKnownHarnesses(append([]string{}, v.context.KnownHarnesses...)),
		workflow.WithKnownExternalTelemetryConnectors(append([]string{}, v.context.KnownExternalTelemetryConnectors...)),
		workflow.WithPreviewFeatures(v.context.AllowPreviewFeatures),
		workflow.WithGaggleRunsOn(v.context.Gaggle.Spec.RunsOn),
		workflow.WithGaggleRequiredCapabilities(v.context.Gaggle.Spec.RequiredCapabilities))
	if err != nil {
		return nil, refusal("compile", "", "spec", bounded(err.Error()))
	}
	// These normal authoring checks deliberately live outside Compile; a
	// structurally valid machine can still read an absent producer output.
	for _, check := range []struct {
		code  string
		check func(workflow.Definition) []string
	}{
		{"contracts", workflow.CheckStageContracts},
		{"paths", workflow.CheckPathSimulation},
		{"required_inputs", workflow.CheckStageRequiredInputs},
		{"timeouts", workflow.CheckStageTimeoutCoherence},
	} {
		if problems := check.check(def); len(problems) != 0 {
			return nil, refusal(check.code, "", "spec", bounded(strings.Join(problems, "; ")))
		}
	}
	return machine, nil
}

func (v *Validator) pinProposal(wf apiv1.Workflow, def workflow.Definition, goobers map[string]apiv1.GooberSpec) ([]engine.PinnedPlacement, error) {
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{v.context.Gaggle}, Workflows: []apiv1.Workflow{wf}}
	for name, goober := range goobers {
		g := apiv1.Goober{Spec: goober}
		g.Name = name
		set.Goobers = append(set.Goobers, g)
	}
	if err := instance.CheckProviderCapabilityRequirements(set); err != nil {
		return nil, refusal("provider", "", "spec.requires", bounded(err.Error()))
	}
	inventory := v.context.Config.PlacementInventory(runtime.GOOS)
	requirements, err := workflow.IsolationStagePlacements(def, v.context.Gaggle.Spec, goobers, inventory.ClassMandates, inventory.SelfExecutionDenied)
	if err != nil {
		return nil, refusal("placement", "", "spec", bounded(err.Error()))
	}
	placement := runnersolve.Solve(inventory, requirements)
	if v.context.Backend == BackendRunner {
		placement = runnersolve.SolveExecutable(inventory, requirements)
	}
	if failures := placement.Unsatisfiable(); len(failures) != 0 {
		return nil, refusal("placement", failures[0].Stage, "runsOn", bounded(failures[0].Unsat.Diagnostic))
	}
	pins, err := bootstrap.PinStagePlacements(v.context.Config, set, wf.Spec.Gaggle, def)
	if err != nil {
		return nil, refusal("placement", "", "runsOn", bounded(err.Error()))
	}
	if v.context.Backend == BackendEngine {
		if err := engine.RefusePlacedDefinition(wf.Name, wf.Spec, pins); err != nil {
			return nil, refusal("backend", "", "spec", bounded(err.Error()))
		}
	}
	// Runner specs can contain slices borrowed from the pinned config. The
	// returned proposal belongs to its caller and must not expose those slices.
	pinBytes, err := json.Marshal(pins)
	if err != nil {
		return nil, fmt.Errorf("snapshot child placements: %w", err)
	}
	var independentPins []engine.PinnedPlacement
	if err := json.Unmarshal(pinBytes, &independentPins); err != nil {
		return nil, err
	}
	return independentPins, nil
}

func (v *Validator) checkAuthority(wf apiv1.Workflow) error {
	policy := v.context.ParentTask.ChildWorkflows
	checkGoober := func(name, stage, field string) (apiv1.GooberSpec, error) {
		g, ok := v.context.Goobers[name]
		if !ok || !slices.Contains(policy.AllowedGoobers, name) || (g.Gaggle != "" && g.Gaggle != wf.Spec.Gaggle) {
			return apiv1.GooberSpec{}, refusal("goober", stage, field, "Goober is not available within the pinned child grant")
		}
		// Copilot model authentication can also carry GitHub publication authority.
		// A model-only declaration does not prove that this credential is isolated.
		if (!policy.AllowPRPublication || !v.context.AllowPRPublication) && (g.Harness == "" || g.Harness == apiv1.HarnessCopilot) {
			return apiv1.GooberSpec{}, refusal("credential_isolation", stage, field, "child model authentication may carry GitHub publication authority; a separate model-only authentication path is required")
		}
		return g, nil
	}
	checkCaps := func(grants []string, stage, field string) error {
		for _, grant := range grants {
			if !slices.Contains(policy.AllowedCapabilities, grant) || !slices.Contains(v.context.GrantedCapabilities, grant) {
				return refusal("capability", stage, field, "effective capability exceeds the pinned child or enclosing grant")
			}
			if (!policy.AllowPRPublication || !v.context.AllowPRPublication) && publicationCapability(grant) {
				return refusal("publication", stage, field, "publication-capable credentials require both child and enclosing publication permission")
			}
		}
		return nil
	}
	for _, task := range wf.Spec.Tasks {
		seen := make(map[string]bool)
		for _, name := range task.ContextFrom {
			if seen[name] {
				return refusal("context", task.Name, "contextFrom", "contextFrom cannot repeat a producer")
			}
			seen[name] = true
		}
		if task.NestedAgentPolicy != nil {
			return refusal("nested_unsupported", task.Name, "nestedAgentPolicy", "declared nested-agent delegation is not supported by child admission yet")
		}
		if task.OutboxMirrorPath != "" {
			return refusal("scope", task.Name, "outboxMirrorPath", "child cannot choose a host outbox path")
		}
		if task.Type == apiv1.TaskAgentic {
			if _, err := checkGoober(task.Goober, task.Name, "goober"); err != nil {
				return err
			}
		}
		if err := checkCaps(task.Capabilities, task.Name, "capabilities"); err != nil {
			return err
		}
	}
	for _, gate := range wf.Spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil {
			continue
		}
		goober, err := checkGoober(gate.Agentic.Goober, gate.Name, "agentic.goober")
		if err != nil {
			return err
		}
		// Reviewers have no stage-level grant list: the runtime uses the full
		// pinned Goober grant, unlike agentic tasks' declared subsets.
		if err := checkCaps(goober.Capabilities, gate.Name, "agentic.goober.capabilities"); err != nil {
			return err
		}
	}
	return nil
}

func publicationCapability(name string) bool {
	switch capability.Capability(name) {
	case capability.RepoPush, capability.ConfigRepoWrite, capability.ProviderPRWrite,
		capability.GitHubPRWrite, capability.ADOPRWrite, capability.GitHubPRMerge, capability.ADOPRComplete:
		return true
	default:
		return false
	}
}

func singleDocument(source []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return refusal("syntax", "", "", "expected one YAML or JSON Workflow mapping")
	}
	// Aliases are unnecessary in generated proposals and can expand far beyond
	// the byte bound during YAML-to-JSON conversion. Refuse before expansion.
	var check func(*yaml.Node) bool
	check = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode {
			return false
		}
		for _, c := range n.Content {
			if !check(c) {
				return false
			}
		}
		return true
	}
	if !check(&document) {
		return refusal("syntax", "", "", "YAML aliases are not accepted in child proposals")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return refusal("documents", "", "", "exactly one Workflow document is required")
	}
	return nil
}

func digest(raw []byte) string {
	value := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(value[:])
}
func bounded(message string) string {
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}
