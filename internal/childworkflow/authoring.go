package childworkflow

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

// ParentAuthoringSkillName is reserved for the host-supplied authoring package.
const ParentAuthoringSkillName = "goobers-child-workflows"

const maxAuthoringCatalogBytes = 64 << 10

//go:embed authoring/SKILL.md
var parentAuthoringSkill string

// AuthoringCatalog is an advisory projection, never an execution grant. Its
// explicit fields prevent credentials, instructions and launcher configuration
// from crossing from instance configuration into the model's workspace.
type AuthoringCatalog struct {
	SchemaVersion       string             `json:"schemaVersion"`
	DSLVersion          string             `json:"dslVersion"`
	Gaggle              string             `json:"gaggle"`
	ConfigDigest        string             `json:"configDigest"`
	PolicyDigest        string             `json:"policyDigest"`
	NewSubmissions      bool               `json:"newSubmissionsAvailable"`
	AllowPRPublication  bool               `json:"allowPRPublication"`
	AllowedCapabilities []string           `json:"allowedCapabilities"`
	Goobers             []AuthoringGoober  `json:"goobers"`
	Runners             []AuthoringRunner  `json:"runners"`
	PlacementFloor      apiv1.GaggleRunsOn `json:"placementFloor"`
	MaxChildren         int32              `json:"maxChildren"`
	MaxProposalBytes    int                `json:"maxProposalBytes"`
	MaxStates           int                `json:"maxStates"`
	// PreviewFeatures is the parent's pinned preview policy. A child may use
	// preview constructs (selective parallels, maps) only when it is true.
	PreviewFeatures bool `json:"previewFeatures"`
	// Constructs is the construct-selection guide
	// (plan-driven-dynamic-fan-out.md §3) with each row's availability to
	// this child, so the author can pick the cheapest construct that fits.
	Constructs []AuthoringConstruct `json:"constructs"`
}

// AuthoringConstruct is one row of the construct-selection guide.
type AuthoringConstruct struct {
	Need      string `json:"need"`
	Construct string `json:"construct"`
	DecidedBy string `json:"decidedBy"`
	FixedBy   string `json:"fixedByAuthor"`
	Preview   bool   `json:"preview,omitempty"`
	Available bool   `json:"available"`
	// Unavailable says why Available is false.
	Unavailable string `json:"unavailable,omitempty"`
}

// authoringConstructs projects the §3 guide in its documented order. A
// generated child never opens child workflows itself, and preview constructs
// follow the parent's pinned preview policy.
func authoringConstructs(preview bool) []AuthoringConstruct {
	rows := []AuthoringConstruct{
		{Need: "One path out of several", Construct: "gate branches", DecidedBy: "Gate verdict", FixedBy: "All paths"},
		{Need: "A fixed set of branches, all run", Construct: "static parallel (spec.parallels)", DecidedBy: "Nothing; every branch runs", FixedBy: "Branches and width"},
		{Need: "A fixed set of different branches, a subset run", Construct: "selective parallel (spec.parallels[].selection, branches[].optional)", DecidedBy: "Selection artifact", FixedBy: "Branches; width at most the declared count", Preview: true},
		{Need: "The same task over a data-determined list", Construct: "map (spec.maps)", DecidedBy: "Roster artifact", FixedBy: "Task template; width at most 32", Preview: true},
		{Need: "A composition nobody can know until run time", Construct: "child workflow (task.childWorkflows)", DecidedBy: "The parent agent writes a Workflow", FixedBy: "Policy ceiling only"},
		{Need: "Skip the whole fan-out", Construct: "gate before the parallel or map", DecidedBy: "Gate verdict", FixedBy: "Both paths"},
	}
	for i := range rows {
		switch {
		case strings.HasPrefix(rows[i].Construct, "child workflow"):
			rows[i].Unavailable = "a generated child cannot open child workflows"
		case rows[i].Preview && !preview:
			rows[i].Unavailable = "preview; the parent's pinned policy does not allow preview features"
		default:
			rows[i].Available = true
		}
	}
	return rows
}

// AuthoringGoober exposes declared composition inputs without persona content.
type AuthoringGoober struct {
	Name          string   `json:"name"`
	Harness       string   `json:"harness"`
	Capabilities  []string `json:"capabilities"`
	PolicyActions []string `json:"policyActions,omitempty"`
}

// AuthoringRunner describes a declared Linux image runner, not live readiness.
// Image references, endpoints, environment, auth and mounts are deliberately absent.
type AuthoringRunner struct {
	Name         string   `json:"name"`
	OS           string   `json:"os"`
	CPU          string   `json:"cpu,omitempty"`
	Memory       string   `json:"memory,omitempty"`
	Disk         string   `json:"disk,omitempty"`
	Shell        bool     `json:"shell"`
	Harnesses    []string `json:"harnesses"`
	Capabilities []string `json:"capabilities"`
	Restrictions []string `json:"restrictions"`
}

// AttachParentAuthoringSkill adds bounded guidance under the caller's prepared
// stage lease, before kit custody is committed. Neither an existing package nor
// caller-owned maps/slices are overwritten. Validation/start remain authoritative.
func AttachParentAuthoringSkill(kit *agentickit.Kit, authority Authority) error {
	if kit == nil || kit.Envelope.ChildWorkflowOrigin == nil || kit.Envelope.RunID != authority.Origin.RunID || kit.Envelope.Gaggle != authority.Origin.Gaggle || kit.Envelope.Goober != authority.Admission.ParentTask.Goober {
		return ErrAuthorityUnavailable
	}
	if *kit.Envelope.ChildWorkflowOrigin != (apiv1.ChildWorkflowOrigin{StageOccurrence: authority.Origin.StageOccurrence, AttemptID: authority.Origin.AttemptID}) || kit.Envelope.ConfigGeneration != authority.ConfigGeneration || authority.Origin.ConfigDigest != authority.Admission.ConfigDigest {
		return ErrAuthorityUnavailable
	}
	spec, ok := kit.Goobers[kit.Envelope.Goober]
	if !ok || (spec.Harness != apiv1.HarnessClaudeCode && spec.Harness != apiv1.HarnessCodex && spec.Harness != apiv1.HarnessCopilot) {
		return ErrAuthorityUnavailable
	}
	if _, exists := kit.SkillPackages[ParentAuthoringSkillName]; exists || slices.Contains(spec.Skills, ParentAuthoringSkillName) {
		return errors.New("parent authoring skill name is reserved")
	}
	data, err := parentAuthoringCatalog(authority)
	if err != nil {
		return err
	}
	packages := maps.Clone(kit.SkillPackages)
	if packages == nil {
		packages = make(map[string][]workflow.SkillFile)
	}
	packages[ParentAuthoringSkillName] = []workflow.SkillFile{{Path: "SKILL.md", Content: parentAuthoringSkill}, {Path: "catalog.json", Content: string(data)}}
	spec.Skills = append(slices.Clone(spec.Skills), ParentAuthoringSkillName)
	goobers := maps.Clone(kit.Goobers)
	goobers[kit.Envelope.Goober] = spec
	instructions := maps.Clone(kit.Instructions)
	if instructions == nil {
		instructions = make(map[string]string)
	}
	directory := ".claude/skills/"
	if spec.Harness == apiv1.HarnessCodex {
		directory = ".agents/skills/"
	} else if spec.Harness == apiv1.HarnessCopilot {
		directory = ".github/skills/"
	}
	instructions[kit.Envelope.Goober] += "\n\nBefore composing a child workflow, read " + directory + ParentAuthoringSkillName + "/SKILL.md and its catalog.json. The catalog is advisory; the child tools enforce current authority.\n"
	kit.SkillPackages, kit.Goobers, kit.Instructions = packages, goobers, instructions
	return nil
}

func parentAuthoringCatalog(authority Authority) ([]byte, error) {
	input := authority.Admission
	policy := input.ParentTask.ChildWorkflows
	if policy == nil || input.Config == nil || authority.Origin.Gaggle != input.Gaggle.Name || input.ConfigDigest == "" || len(policy.AllowedGoobers) > 128 {
		return nil, ErrAuthorityUnavailable
	}
	ceiling := credentials.NewChildCeiling(policy.AllowPRPublication && input.AllowPRPublication, input.GrantedCapabilities, policy.AllowedCapabilities)
	catalog := AuthoringCatalog{SchemaVersion: "goobers-child-authoring/v1", DSLVersion: supportmatrix.V31DSLVersion, Gaggle: input.Gaggle.Name, ConfigDigest: input.ConfigDigest, PolicyDigest: authority.Origin.PolicyDigest,
		AllowPRPublication: ceiling.AllowPublication, AllowedCapabilities: ceiling.AllowedKeys, MaxChildren: policy.EffectiveMaxChildren(), MaxProposalBytes: MaxProposalBytes, MaxStates: MaxProposalStates,
		PreviewFeatures: input.AllowPreviewFeatures, Constructs: authoringConstructs(input.AllowPreviewFeatures),
		Goobers: []AuthoringGoober{}, Runners: []AuthoringRunner{}}
	_, validationErr := NewValidator(input)
	catalog.NewSubmissions = validationErr == nil
	if input.Gaggle.Spec.RunsOn != nil {
		catalog.PlacementFloor = *input.Gaggle.Spec.RunsOn.DeepCopy()
	}
	catalog.PlacementFloor.Capabilities = sortedAuthoringStrings(append(slices.Clone(catalog.PlacementFloor.Capabilities), input.Gaggle.Spec.RequiredCapabilities...))
	for _, name := range sortedAuthoringStrings(policy.AllowedGoobers) {
		spec, available := input.Goobers[name]
		if !available || (spec.Gaggle != "" && spec.Gaggle != input.Gaggle.Name) {
			continue
		}
		var allowed []string
		for _, cap := range ceiling.AllowedKeys {
			if slices.Contains(spec.Capabilities, cap) {
				allowed = append(allowed, cap)
			}
		}
		catalog.Goobers = append(catalog.Goobers, AuthoringGoober{Name: name, Harness: string(spec.Harness), Capabilities: allowed, PolicyActions: sortedAuthoringStrings(spec.PolicyActions)})
	}
	for _, runner := range input.Config.ResolvedRunners() {
		kind, err := instance.ClassifyRunnerHost(runner.Host)
		if err != nil || kind != instance.RunnerHostImage || runner.Provides.OS != instance.RunnerOSLinux {
			continue
		}
		if len(catalog.Runners) >= 128 {
			return nil, errors.New("parent authoring runner catalog exceeds bound")
		}
		p := runner.Provides
		entry := AuthoringRunner{Name: runner.Name, OS: string(p.OS), CPU: p.CPU, Memory: p.Memory, Disk: p.Disk, Shell: p.Shell, Harnesses: sortedAuthoringStrings(p.Harnesses), Capabilities: sortedAuthoringStrings(p.Capabilities), Restrictions: []string{}}
		for _, restriction := range runner.Restrictions {
			entry.Restrictions = append(entry.Restrictions, string(restriction))
		}
		entry.Restrictions = sortedAuthoringStrings(entry.Restrictions)
		catalog.Runners = append(catalog.Runners, entry)
	}
	slices.SortFunc(catalog.Runners, func(a, b AuthoringRunner) int { return cmp.Compare(a.Name, b.Name) })
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil || len(data) > maxAuthoringCatalogBytes {
		return nil, errors.New("parent authoring catalog exceeds bound")
	}
	return data, nil
}

func sortedAuthoringStrings(values []string) []string {
	result := append([]string{}, values...)
	slices.Sort(result)
	return slices.Compact(result)
}
