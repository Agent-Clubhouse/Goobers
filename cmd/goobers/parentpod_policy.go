package main

import (
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func hasChildParent(spec apiv1.WorkflowSpec) bool {
	for _, task := range spec.Tasks {
		if task.ChildWorkflows != nil {
			return true
		}
	}
	return false
}

// Placement constraints apply only to stages opting into child workflows.
// Ordinary stages and gates keep their existing execution policy and placement.
func containedParentShape(cfg *instance.Config, goobers map[string]apiv1.GooberSpec, machine *workflow.Machine) error {
	if machine == nil || !hasChildParent(machine.Def.Spec) {
		return fmt.Errorf("parent workflow is not opted in")
	}
	refuse := func(reason string) error {
		return fmt.Errorf("%w: %s", workflow.ErrChildWorkflowExecutionUnsupported, reason)
	}
	if cfg == nil || !cfg.EngineProjectionEnabled() || cfg.API.PodTokenKeyFile == "" {
		return refuse("contained parent requires authenticated worker transport")
	}
	spec := machine.Def.Spec
	for _, task := range spec.Tasks {
		if task.ChildWorkflows == nil {
			continue
		}
		if task.Type != apiv1.TaskAgentic || task.EffectiveWorkspace() != apiv1.WorkspaceRepo {
			return refuse("a child-enabled stage must be agentic and use a managed repository workspace")
		}
		if !containedParentRepoFrom(machine, task) {
			return refuse("contained parent repoFrom must name its repository producers")
		}
		goober, ok := goobers[task.Goober]
		if !ok || (goober.Harness != apiv1.HarnessClaudeCode && goober.Harness != apiv1.HarnessCodex) || len(goober.MCPServers) != 0 {
			return refuse("contained parent requires an API-authenticated Claude or Codex profile without BYO MCP")
		}
		if goober.Harness == apiv1.HarnessCodex && harness.CodexUsesAmbientChatGPT(goober.HarnessOptions) {
			return refuse("contained parent cannot inherit ambient ChatGPT authentication")
		}
	}
	return nil
}

func containedParentRepoFrom(machine *workflow.Machine, task apiv1.Task) bool {
	if len(task.RepoFrom) == 0 {
		return true // The runner may supply its ordinary upstream workspace.
	}
	for _, name := range task.RepoFrom {
		producer, found := machine.Task(name)
		if !found || producer.EffectiveWorkspace() != apiv1.WorkspaceRepo {
			return false
		}
	}
	return true
}

func containedParentPlacements(cfg *instance.Config, set *instance.ConfigSet, machine *workflow.Machine) ([]dispatcher.PinnedPlacement, error) {
	if set == nil || machine == nil {
		return nil, fmt.Errorf("parent source catalog unavailable")
	}
	goobers, err := resolveGoobersForGaggle(set, machine.Def.Spec.Gaggle)
	if err != nil {
		return nil, err
	}
	if err := containedParentShape(cfg, goobers, machine); err != nil {
		return nil, err
	}
	for _, gaggle := range set.Gaggles {
		if gaggle.Name == machine.Def.Spec.Gaggle {
			if len(gaggle.Spec.AdditionalRepos) != 0 {
				return nil, fmt.Errorf("contained parent additional repositories are not yet supported")
			}
			if configured, ok := configuredRepoForProject(cfg, gaggle.Spec.Project); ok && configured.Pinned() {
				return nil, fmt.Errorf("contained parent requires a managed worktree, not a pinned checkout")
			}
		}
	}
	pins, err := bootstrap.PinStagePlacements(cfg, set, machine.Def.Spec.Gaggle, machine.Def)
	if err != nil {
		return nil, err
	}
	selected := make([]dispatcher.PinnedPlacement, 0)
	for _, task := range machine.Def.Spec.Tasks {
		if task.ChildWorkflows == nil {
			continue
		}
		i := slices.IndexFunc(pins, func(pin dispatcher.PinnedPlacement) bool { return pin.Stage == task.Name })
		if i < 0 || pins[i].Self || pins[i].Queue == "" || len(pins[i].Eligible) == 0 {
			return nil, fmt.Errorf("contained parent stage %q has no worker placement", task.Name)
		}
		selected = append(selected, pins[i])
		for _, runner := range pins[i].Eligible {
			if runner.HostKind != instance.RunnerHostImage || runner.OS != "linux" {
				return nil, fmt.Errorf("contained parent requires Linux image runners")
			}
		}
	}
	return selected, nil
}
