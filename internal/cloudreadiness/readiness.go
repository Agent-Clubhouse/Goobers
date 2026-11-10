// Package cloudreadiness assesses whether every workflow in an instance can
// execute on the engine-only cloud runtime.
package cloudreadiness

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engineadmission"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

const (
	// SchemaVersion identifies the stable JSON report contract.
	SchemaVersion = "goobers.dev/cloud-readiness/v1"
	// CodeFeature identifies an engine-unsupported DSL feature.
	CodeFeature = "RUNTIME001"
	// CodePlacement identifies a stage that requires control-plane-host execution.
	CodePlacement = "RUNTIME002"
	// CodeDSL identifies a local-runner-only DSL version.
	CodeDSL = "RUNTIME003"
	// CodeControlPlane identifies a workflow setting the cloud control plane
	// cannot honour yet, although the engine can execute the workflow.
	CodeControlPlane = "RUNTIME004"
)

// Action describes how an operator can remove a blocker.
type Action struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	Issue       int    `json:"issue,omitempty"`
	IssueURL    string `json:"issueUrl,omitempty"`
}

// Blocker is one workflow-specific cloud eligibility failure.
type Blocker struct {
	Code    string `json:"code"`
	Feature string `json:"feature,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Action  Action `json:"action"`
}

// Workflow contains every blocker for one configured workflow.
type Workflow struct {
	Gaggle     string    `json:"gaggle"`
	Name       string    `json:"name"`
	DSLVersion string    `json:"dslVersion"`
	Blockers   []Blocker `json:"blockers"`
}

// Occurrence locates one instance of a grouped blocker.
type Occurrence struct {
	Gaggle   string `json:"gaggle"`
	Workflow string `json:"workflow"`
	Stage    string `json:"stage,omitempty"`
	Path     string `json:"path"`
}

// BlockerGroup groups blocker occurrences that share one remedy.
type BlockerGroup struct {
	Key         string       `json:"key"`
	Code        string       `json:"code"`
	Feature     string       `json:"feature,omitempty"`
	Action      Action       `json:"action"`
	Occurrences []Occurrence `json:"occurrences"`
}

// Summary contains whole-instance blocker counts.
type Summary struct {
	BlockedWorkflows int `json:"blockedWorkflows"`
	TotalBlockers    int `json:"totalBlockers"`
	ConfigFixes      int `json:"configFixes"`
	WaitingOnP4      int `json:"waitingOnP4"`
	// WaitingOnControlPlane counts blockers waiting on cloud control-plane work.
	WaitingOnControlPlane int   `json:"waitingOnControlPlane,omitempty"`
	P4Issues              []int `json:"p4Issues"`
}

// Result is the stable whole-instance cloud readiness report.
type Result struct {
	SchemaVersion string         `json:"schemaVersion"`
	Target        string         `json:"target"`
	CloudReady    bool           `json:"cloudReady"`
	Summary       Summary        `json:"summary"`
	Workflows     []Workflow     `json:"workflows"`
	Blockers      []BlockerGroup `json:"blockers"`
}

// FeatureBlocker identifies the DSL path and P4 issue for an engine refusal.
type FeatureBlocker struct {
	Path  string
	Issue int
	// Remaining, when set, names what still blocks the feature beyond the
	// tracking issue, for the operator's action.
	Remaining string
}

var featureBlockers = map[workflow.FeatureID]FeatureBlocker{
	"workflow.spec.parallels": {Path: "spec.parallels", Issue: 7753},
	"task.experiment":         {Path: "task.experiment", Issue: 7763},
}

// EngineFeatureBlocker returns the P4 metadata for one refused feature.
func EngineFeatureBlocker(id workflow.FeatureID) (FeatureBlocker, bool) {
	blocker, ok := featureBlockers[id]
	return blocker, ok
}

// EngineUnsupportedFeatureIDs returns refused registry features in stable order.
func EngineUnsupportedFeatureIDs() []workflow.FeatureID {
	ids := make([]workflow.FeatureID, 0, len(featureBlockers))
	for id := range featureBlockers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Assess reports every engine-only cloud blocker in an instance config.
func Assess(cfg *instance.Config, set *instance.ConfigSet) Result {
	result := Result{
		SchemaVersion: SchemaVersion,
		Target:        "cloud",
		CloudReady:    true,
		Workflows:     []Workflow{},
		Blockers:      []BlockerGroup{},
	}
	if cfg == nil || set == nil {
		return result
	}
	for i := range set.Workflows {
		current := &set.Workflows[i]
		blockers := workflowBlockers(cfg, set, current)
		if len(blockers) == 0 {
			continue
		}
		result.CloudReady = false
		result.Workflows = append(result.Workflows, Workflow{
			Gaggle: current.Spec.Gaggle, Name: current.Name,
			DSLVersion: current.DSLVersion, Blockers: blockers,
		})
	}
	sort.Slice(result.Workflows, func(i, j int) bool {
		if result.Workflows[i].Gaggle != result.Workflows[j].Gaggle {
			return result.Workflows[i].Gaggle < result.Workflows[j].Gaggle
		}
		return result.Workflows[i].Name < result.Workflows[j].Name
	})
	result.Summary, result.Blockers = summarize(result.Workflows)
	return result
}

func workflowBlockers(cfg *instance.Config, set *instance.ConfigSet, current *apiv1.Workflow) []Blocker {
	def := workflow.Definition{
		Name: current.Name, Version: 1, DSLVersion: current.DSLVersion, Spec: current.Spec,
	}
	placements, err := bootstrap.PinChildStagePlacements(cloudAssessmentConfig(cfg), set, current.Spec.Gaggle, def)
	blockers := unsupportedFeatureBlockers(current.Name, current.Spec, placements)
	if err != nil {
		// A workflow the inventory cannot place would fall back to the
		// control-plane host, which the cloud does not have: fail closed.
		blockers = append(blockers, Blocker{
			Code: CodePlacement, Path: "/spec", Message: err.Error(),
			Action: Action{
				Kind:        "config",
				Description: "declare a non-self runner that satisfies the workflow's placement requirements",
			},
		})
	} else {
		blockers = append(blockers, placementBlockers(current.Spec, placements)...)
	}

	if current.DSLVersion != supportmatrix.V3DSLVersion &&
		current.DSLVersion != supportmatrix.V31DSLVersion {
		blockers = append(blockers, Blocker{
			Code: CodeDSL, Path: "/dslVersion",
			Message: fmt.Sprintf("DSL %s is local-runner-only and cannot run in cloud", current.DSLVersion),
			Action: Action{
				Kind: "config", Description: "migrate the workflow to DSL 3.0",
				Command: "goobers fix --to 3.0",
			},
		})
	}
	sort.SliceStable(blockers, func(i, j int) bool {
		return blockers[i].Code < blockers[j].Code
	})
	return blockers
}

func unsupportedFeatureBlockers(name string, spec apiv1.WorkflowSpec, placements []dispatcher.PinnedPlacement) []Blocker {
	var blockers []Blocker
	err := engineadmission.RefusePlacedDefinition(name, spec, placements)
	if err == nil {
		return blockers
	}
	var refused *engineadmission.UnsupportedFeaturesError
	if !errors.As(err, &refused) {
		return []Blocker{genericFeatureBlocker("", "", "/spec", err.Error())}
	}
	for _, refusal := range refused.Refusals {
		id, ok := featureIDForPath(refusal.Feature)
		if !ok {
			blockers = append(blockers, genericFeatureBlocker(
				refusal.Feature, refusal.Stage, genericFeaturePath(spec, refusal.Feature, refusal.Stage), refusal.Error(),
			))
			continue
		}
		if id == "workflow.spec.parallels" {
			for i, parallel := range spec.Parallels {
				blockers = append(blockers, featureBlocker(id, parallel.Name, fmt.Sprintf("/spec/parallels/%d", i)))
			}
			continue
		}
		blockers = append(blockers, featureBlocker(id, refusal.Stage, featurePath(spec, id, refusal.Stage)))
	}
	return blockers
}

func genericFeatureBlocker(feature, stage, path, message string) Blocker {
	return Blocker{
		Code: CodeFeature, Feature: feature, Stage: stage, Path: path,
		Message: message,
		Action: Action{
			Kind:        "engine",
			Description: "resolve the engine refusal before cloud cutover",
		},
	}
}

func genericFeaturePath(spec apiv1.WorkflowSpec, feature, stage string) string {
	for i, mapped := range spec.Maps {
		if feature == "spec.maps" && mapped.Name == stage {
			return fmt.Sprintf("/spec/maps/%d", i)
		}
	}
	for i, gate := range spec.Gates {
		if feature == "gate.evaluator.human" && gate.Name == stage {
			return fmt.Sprintf("/spec/gates/%d/evaluator", i)
		}
	}
	for i, task := range spec.Tasks {
		if task.Name == stage && strings.HasPrefix(feature, "task.") {
			return fmt.Sprintf("/spec/tasks/%d/%s", i,
				strings.ReplaceAll(strings.TrimPrefix(feature, "task."), ".", "/"))
		}
	}
	if feature == "" {
		return "/spec"
	}
	return "/" + strings.ReplaceAll(feature, ".", "/")
}

func featureBlocker(id workflow.FeatureID, stage, path string) Blocker {
	meta := featureBlockers[id]
	feature := meta.Path
	description := fmt.Sprintf("wait for engine parity in #%d", meta.Issue)
	if meta.Remaining != "" {
		description += ": " + meta.Remaining
	}
	return Blocker{
		Code: CodeFeature, Feature: feature, Stage: stage, Path: path,
		Message: fmt.Sprintf("engine does not support %s", feature),
		Action: Action{
			Kind: "p4", Description: description,
			Issue:    meta.Issue,
			IssueURL: fmt.Sprintf("https://github.com/Agent-Clubhouse/Goobers/issues/%d", meta.Issue),
		},
	}
}

func featurePath(spec apiv1.WorkflowSpec, id workflow.FeatureID, stage string) string {
	for i, parallel := range spec.Parallels {
		if parallel.Name == stage && id == "workflow.spec.parallels.selection" {
			return fmt.Sprintf("/spec/parallels/%d/selection", i)
		}
	}
	for i, mapped := range spec.Maps {
		if mapped.Name == stage && id == "workflow.spec.maps" {
			return fmt.Sprintf("/spec/maps/%d", i)
		}
	}
	for i, gate := range spec.Gates {
		if gate.Name == stage && id == "gate.evaluator.human" {
			return fmt.Sprintf("/spec/gates/%d/evaluator", i)
		}
	}
	for i, task := range spec.Tasks {
		if task.Name != stage {
			continue
		}
		suffix := strings.TrimPrefix(string(id), "task.")
		return fmt.Sprintf("/spec/tasks/%d/%s", i, strings.ReplaceAll(suffix, ".", "/"))
	}
	return "/" + strings.ReplaceAll(featureBlockers[id].Path, ".", "/")
}

func placementBlockers(spec apiv1.WorkflowSpec, placements []dispatcher.PinnedPlacement) []Blocker {
	var blockers []Blocker
	pinned := make(map[string]bool, len(placements))
	for _, placement := range placements {
		pinned[placement.Stage] = true
		if !placement.Self {
			continue
		}
		blockers = append(blockers, Blocker{
			Code: CodePlacement, Stage: placement.Stage, Path: stageRunsOnPath(spec, placement.Stage),
			Message: fmt.Sprintf("stage %q is pinned to the control-plane host", placement.Stage),
			Action: Action{
				Kind: "config", Description: "add runsOn that selects a non-self runner",
			},
		})
	}
	for i, gate := range spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || pinned[gate.Name] {
			continue
		}
		blockers = append(blockers, Blocker{
			Code: CodePlacement, Stage: gate.Name, Path: fmt.Sprintf("/spec/gates/%d/runsOn", i),
			Message: fmt.Sprintf("agentic gate %q has no runsOn and would execute on the control-plane host", gate.Name),
			Action:  Action{Kind: "config", Description: "add runsOn to the agentic gate"},
		})
	}
	return blockers
}

func stageRunsOnPath(spec apiv1.WorkflowSpec, stage string) string {
	for i, task := range spec.Tasks {
		if task.Name == stage {
			return fmt.Sprintf("/spec/tasks/%d/runsOn", i)
		}
	}
	for i, gate := range spec.Gates {
		if gate.Name == stage {
			return fmt.Sprintf("/spec/gates/%d/runsOn", i)
		}
	}
	return "/spec"
}

func featureIDForPath(path string) (workflow.FeatureID, bool) {
	for id, blocker := range featureBlockers {
		if blocker.Path == path {
			return id, true
		}
	}
	return "", false
}

func summarize(workflows []Workflow) (Summary, []BlockerGroup) {
	summary := Summary{BlockedWorkflows: len(workflows), P4Issues: []int{}}
	groups := make(map[string]*BlockerGroup)
	issues := make(map[int]bool)
	for _, wf := range workflows {
		for _, blocker := range wf.Blockers {
			summary.TotalBlockers++
			switch blocker.Action.Kind {
			case "p4":
				summary.WaitingOnP4++
				issues[blocker.Action.Issue] = true
			case "control-plane":
				summary.WaitingOnControlPlane++
			default:
				summary.ConfigFixes++
			}
			key := blocker.Code + ":" + blocker.Feature
			if blocker.Code == CodePlacement {
				key = blocker.Code + ":" + blocker.Action.Description
			}
			group := groups[key]
			if group == nil {
				group = &BlockerGroup{
					Key: key, Code: blocker.Code, Feature: blocker.Feature,
					Action: blocker.Action, Occurrences: []Occurrence{},
				}
				groups[key] = group
			}
			group.Occurrences = append(group.Occurrences, Occurrence{
				Gaggle: wf.Gaggle, Workflow: wf.Name, Stage: blocker.Stage, Path: blocker.Path,
			})
		}
	}
	for issue := range issues {
		summary.P4Issues = append(summary.P4Issues, issue)
	}
	sort.Ints(summary.P4Issues)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]BlockerGroup, 0, len(keys))
	for _, key := range keys {
		out = append(out, *groups[key])
	}
	return summary, out
}

// cloudAssessmentConfig returns cfg with every self runner that declares no OS
// pinned to Linux. The solver otherwise substitutes the OS of the machine
// running the assessment, so the same instance would report different
// blockers on a laptop and in CI. Cloud workers are Linux pods, and the
// self runner is only ever assessed as a blocker, so the host's OS is
// irrelevant to cloud eligibility.
func cloudAssessmentConfig(cfg *instance.Config) *instance.Config {
	out := *cfg
	out.Runners = append([]instance.RunnerEntry(nil), cfg.ResolvedRunners()...)
	for i := range out.Runners {
		if out.Runners[i].Host == instance.RunnerHostSelfName && out.Runners[i].Provides.OS == "" {
			out.Runners[i].Provides.OS = instance.RunnerOSLinux
		}
	}
	return &out
}
