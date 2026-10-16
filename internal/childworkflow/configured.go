package childworkflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

// GooberAdmission reuses the host's normal offline harness/model/MCP admission.
// Implementations must defer discovery and must not resolve credentials, start
// subprocesses, or invoke model/network preflights for this advisory operation.
type GooberAdmission func(*instance.Config, map[string]apiv1.GooberSpec) (AdmittedGoobers, error)

// AdmittedGoobers contains offline-resolved definitions and harness names.
type AdmittedGoobers struct {
	Goobers      map[string]apiv1.GooberSpec
	HarnessNames []string
	Warnings     []string
}

// ConfiguredRequest selects the trusted configured parent and proposal file.
type ConfiguredRequest struct {
	Root           string
	Gaggle         string
	ParentWorkflow string
	ParentStage    string
	ProposalPath   string
	Backend        Backend
}

// ParentSelection identifies a stage within a gaggle-scoped workflow.
type ParentSelection struct {
	Gaggle   string `json:"gaggle"`
	Workflow string `json:"workflow"`
	Stage    string `json:"stage"`
}

// ConfiguredReport reports an offline current-configuration check, never a
// durable admission receipt. ConfigDigest identifies declarative validation
// inputs, not an execution archive of instructions, skills, or credentials.
type ConfiguredReport struct {
	SchemaVersion   string          `json:"schemaVersion"`
	Scope           string          `json:"scope"`
	Advisory        bool            `json:"advisory"`
	Valid           bool            `json:"valid"`
	Parent          ParentSelection `json:"parent"`
	Backend         Backend         `json:"backend"`
	SourceDigest    string          `json:"sourceDigest,omitempty"`
	CanonicalDigest string          `json:"canonicalDigest,omitempty"`
	ConfigDigest    string          `json:"configDigest,omitempty"`
	PolicyDigest    string          `json:"policyDigest,omitempty"`
	WorkflowDigest  string          `json:"workflowDigest,omitempty"`
	Diagnostics     []Diagnostic    `json:"diagnostics"`
	Warnings        []string        `json:"warnings,omitempty"`
}

// ValidateConfigured loads the existing active config/ tree without fetching or
// materializing workflowSource. Only the selected configured parent provides
// policy. Future run-bound callers must use the run's immutable archive and
// NewValidator instead of treating this current-config report as authorization.
func ValidateConfigured(request ConfiguredRequest, admit GooberAdmission) (ConfiguredReport, error) {
	report := ConfiguredReport{
		SchemaVersion: "child-workflow-validation/v1", Scope: "current-config", Advisory: true,
		Parent:  ParentSelection{Gaggle: request.Gaggle, Workflow: request.ParentWorkflow, Stage: request.ParentStage},
		Backend: request.Backend, Diagnostics: []Diagnostic{},
	}
	if request.Gaggle == "" || request.ParentWorkflow == "" || request.ParentStage == "" || request.ProposalPath == "" || admit == nil {
		return report, errors.New("gaggle, parent workflow, parent stage, proposal path, and Goober admission are required")
	}
	if request.Backend != BackendRunner && request.Backend != BackendEngine {
		return report, errors.New("backend must be runner or engine")
	}
	source, err := readProposal(request.ProposalPath)
	if err != nil {
		return report, err
	}
	if len(source) <= MaxProposalBytes {
		report.SourceDigest = digest(source)
	}
	layout := instance.NewLayout(request.Root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return report, err
		}
		return configuredRefusal(report, "config", err.Error()), nil
	}
	set, findings, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		if !errors.Is(err, instance.ErrInvalidConfig) {
			return report, err
		}
		for _, finding := range findings.Issues {
			if finding.Severity == validate.Error {
				report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: string(finding.Code), Field: finding.File, Message: finding.Message})
			}
		}
		return report, nil
	}
	for _, warning := range findings.CLIWarnings() {
		report.Warnings = append(report.Warnings, warning.String())
	}
	context, warnings, err := configuredContext(request, cfg, set, admit)
	if err != nil {
		return configuredRefusal(report, "parent_config", err.Error()), nil
	}
	report.Warnings = append(report.Warnings, warnings...)
	report.ConfigDigest = context.ConfigDigest
	v, err := NewValidator(context)
	if err != nil {
		return configuredRefusal(report, "parent_config", err.Error()), nil
	}
	proposal, err := v.Validate(source)
	if err != nil {
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			report.Diagnostics = invalid.Diagnostics
			return report, nil
		}
		return report, err
	}
	report.Valid = true
	report.SourceDigest = proposal.SourceDigest
	report.CanonicalDigest = proposal.CanonicalDigest
	report.PolicyDigest = proposal.PolicyDigest
	report.WorkflowDigest = proposal.Machine.Digest()
	return report, nil
}

func configuredRefusal(report ConfiguredReport, code, message string) ConfiguredReport {
	report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: code, Message: bounded(message)})
	return report
}

func readProposal(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat proposal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("proposal must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open proposal: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("proposal must be a regular file")
	}
	return io.ReadAll(io.LimitReader(file, MaxProposalBytes+1))
}

func configuredContext(request ConfiguredRequest, cfg *instance.Config, set *instance.ConfigSet, admit GooberAdmission) (AdmissionContext, []string, error) {
	var parent *apiv1.Workflow
	for i := range set.Workflows {
		candidate := &set.Workflows[i]
		if candidate.Name == request.ParentWorkflow && candidate.Spec.Gaggle == request.Gaggle {
			parent = candidate
			break
		}
	}
	if parent == nil {
		return AdmissionContext{}, nil, errors.New("parent workflow is not configured in the selected gaggle")
	}
	if parent.DSLVersion != supportmatrix.V31DSLVersion {
		return AdmissionContext{}, nil, errors.New("configured parent must explicitly pin DSL 3.1")
	}
	var task *apiv1.Task
	for i := range parent.Spec.Tasks {
		if parent.Spec.Tasks[i].Name == request.ParentStage {
			task = &parent.Spec.Tasks[i]
			break
		}
	}
	if task == nil || task.Type != apiv1.TaskAgentic || task.ChildWorkflows == nil {
		return AdmissionContext{}, nil, errors.New("selected parent stage must be a configured agentic task with childWorkflows enabled")
	}
	var gaggle *apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == request.Gaggle {
			gaggle = &set.Gaggles[i]
			break
		}
	}
	if gaggle == nil {
		return AdmissionContext{}, nil, errors.New("parent gaggle is not configured")
	}
	goobers := make(map[string]apiv1.GooberSpec)
	for _, g := range set.Goobers {
		if g.Spec.Gaggle == "" || g.Spec.Gaggle == request.Gaggle {
			goobers[g.Name] = g.Spec
		}
	}
	resolved, err := admit(cfg, goobers)
	if err != nil {
		return AdmissionContext{}, nil, err
	}
	context := AdmissionContext{
		Config: cfg, Gaggle: *gaggle, ParentTask: *task, Backend: request.Backend,
		GrantedCapabilities: slices.Clone(task.Capabilities), AllowPRPublication: task.ChildWorkflows.AllowPRPublication,
		Goobers: resolved.Goobers, KnownHarnesses: resolved.HarnessNames,
		KnownChecks:                      slices.Sorted(maps.Keys(gate.DefaultChecks())),
		KnownExternalTelemetryConnectors: cfg.ExternalTelemetryConnectorNames(),
		AllowPreviewFeatures:             workflow.PreviewFeaturesEnabled(parent.Annotations),
	}
	// Include the resolved declarative inputs and complete parent definition.
	// This is intentionally not presented as a durable generation/archive ID.
	encoded, err := json.Marshal(struct {
		Context AdmissionContext
		Parent  apiv1.Workflow
	}{context, *parent})
	if err != nil {
		return AdmissionContext{}, nil, err
	}
	context.ConfigDigest = digest(encoded)
	return context, resolved.Warnings, nil
}

// ConfiguredAdmission derives advisory validation inputs from a trusted loaded
// configuration snapshot. Runtime callers must additionally verify the exact
// archive and parent journal pins and intersect current applied permissions.
// This function neither authenticates a request nor grants execution authority.
func ConfiguredAdmission(parent ParentSelection, backend Backend, cfg *instance.Config, set *instance.ConfigSet, admit GooberAdmission) (AdmissionContext, error) {
	if cfg == nil || set == nil || admit == nil {
		return AdmissionContext{}, errors.New("configured child admission requires loaded trusted configuration")
	}
	input, _, err := configuredContext(ConfiguredRequest{Gaggle: parent.Gaggle, ParentWorkflow: parent.Workflow, ParentStage: parent.Stage, Backend: backend}, cfg, set, admit)
	return input, err
}
