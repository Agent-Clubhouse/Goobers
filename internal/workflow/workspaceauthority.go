package workflow

import (
	"fmt"
	"slices"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// CheckWorkspaceAuthority checks static misuse without treating an ordinary
// legacy writable workflow as an implicit sandbox.
func CheckWorkspaceAuthority(def Definition) []string {
	var problems []string
	tasks := make(map[string]apiv1.Task)
	sandbox := false
	for _, task := range def.Spec.Tasks {
		tasks[task.Name] = task
		kind := task.Inputs["kind"]
		sandbox = sandbox || kind == "workspace-branch-establish" || kind == "workspace-branch-publish"
		problems = append(problems, workspaceBackendProblems(task)...)
		problems = append(problems, workspaceInputProblems(task)...)
	}
	if sandbox {
		problems = append(problems, workspaceFlowProblems(def, tasks)...)
	}
	sort.Strings(problems)
	return slices.Compact(problems)
}

func workspaceBackendProblems(task apiv1.Task) []string {
	kind := task.Inputs["kind"]
	if kind != "workspace-branch-establish" && kind != "workspace-branch-publish" {
		return nil
	}
	var problems []string
	if _, dynamic := task.InputsFrom["kind"]; dynamic {
		problems = append(problems, fmt.Sprintf("workspace_branch_invalid_stage: stage %q: backend kind must be static, not supplied through inputsFrom", task.Name))
	}
	if task.Type != apiv1.TaskDeterministic || task.Run == nil || !slices.Contains(task.Capabilities, "repo:push") {
		problems = append(problems, fmt.Sprintf("workspace_branch_invalid_stage: stage %q: backend operations require a deterministic task with declared repo:push", task.Name))
	}
	want := apiv1.WorkspaceScratch
	if kind == "workspace-branch-publish" {
		want = apiv1.WorkspaceRepo
	}
	if task.EffectiveWorkspace() != want || task.Run != nil && task.Run.SyncBase || task.ContinueOnError {
		problems = append(problems, fmt.Sprintf("workspace_branch_invalid_stage: stage %q: declare the operation's explicit workspace and disable syncBase and continueOnError", task.Name))
	}
	return problems
}

func workspaceInputProblems(task apiv1.Task) []string {
	var problems []string
	add := func(code, message string) {
		problems = append(problems, fmt.Sprintf("%s: stage %q: %s", code, task.Name, message))
	}
	for _, key := range []string{"workspaceRevision", "workspaceBranchBinding", "workspaceBranchTip"} {
		if _, ok := task.Inputs[key]; ok {
			add("workspace_revision_unauthorized", "workspace authority is typed runtime state, not an input selector")
		}
		if _, ok := task.InputsFrom[key]; ok {
			add("workspace_revision_unauthorized", "workspace authority cannot be selected through inputsFrom")
		}
	}
	if task.EffectiveWorkspace() == apiv1.WorkspaceRepoReadOnly {
		if task.Run != nil && task.Run.SyncBase {
			add("workspace_revision_conflict", "syncBase requires a writable repo workspace")
		}
		for _, key := range []string{"workspaceBranch", "workspaceDelta"} {
			_, direct := task.Inputs[key]
			_, upstream := task.InputsFrom[key]
			if direct || upstream {
				add("workspace_revision_conflict", "repo-readonly cannot consume branch or workspace delta controls")
			}
		}
	}
	return problems
}

type workspaceFlowState struct {
	name            string
	owned, parallel bool
}

func workspaceFlowProblems(def Definition, tasks map[string]apiv1.Task) []string {
	var problems []string
	gates := make(map[string]apiv1.Gate)
	parallels := make(map[string]apiv1.Parallel)
	joins := make(map[string]bool)
	for _, gate := range def.Spec.Gates {
		gates[gate.Name] = gate
	}
	for _, parallel := range def.Spec.Parallels {
		parallels[parallel.Name], joins[parallel.Join] = parallel, true
	}
	queue := []workspaceFlowState{{name: def.Spec.Start}}
	seen := make(map[workspaceFlowState]bool)
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		if current.name == "" || seen[current] {
			continue
		}
		seen[current] = true
		if joins[current.name] {
			current.parallel = false
		}
		next := func(name string) { queue = append(queue, workspaceFlowState{name, current.owned, current.parallel}) }
		if task, ok := tasks[current.name]; ok {
			problems = append(problems, workspaceTaskFlowProblems(task, current)...)
			if task.Inputs["kind"] == "workspace-branch-establish" {
				current.owned = true
			}
			next(task.Next)
		} else if gate, ok := gates[current.name]; ok {
			problems = append(problems, workspaceGateFlowProblems(gate, current)...)
			for _, target := range gate.Branches {
				next(target)
			}
		} else if parallel, ok := parallels[current.name]; ok {
			next(parallel.Join)
			next(parallel.OnFailure)
			for _, branch := range parallel.Branches {
				queue = append(queue, workspaceFlowState{branch.Start, current.owned, true})
			}
		}
	}
	return problems
}

func workspaceTaskFlowProblems(task apiv1.Task, current workspaceFlowState) []string {
	var problems []string
	add := func(code, message string) {
		problems = append(problems, fmt.Sprintf("%s: stage %q: %s", code, task.Name, message))
	}
	kind := task.Inputs["kind"]
	writable := task.EffectiveWorkspace() == "" || task.EffectiveWorkspace().IsWritableRepo()
	if writable && !current.owned {
		add("workspace_branch_establishment_required", "writable sandbox work must be preceded on every path by establishment")
	}
	if current.parallel && (writable || kind == "workspace-branch-establish" || kind == "workspace-branch-publish") {
		add("workspace_branch_parallel_forbidden", "sandbox authority and writable work must remain serial")
	}
	if current.owned && task.EffectiveWorkspace() == apiv1.WorkspaceRepoReadOnly {
		add("workspace_revision_conflict", "repo-readonly cannot consume an established workspaceBranch")
	}
	return problems
}

func workspaceGateFlowProblems(gate apiv1.Gate, current workspaceFlowState) []string {
	if gate.Evaluator != apiv1.EvaluatorAgentic {
		return nil
	}
	var problems []string
	mode := gate.EffectiveWorkspace()
	if (mode == "" || mode.IsWritableRepo()) && (!current.owned || current.parallel) {
		problems = append(problems, fmt.Sprintf("workspace_branch_establishment_required: stage %q: sandbox reviewer requires established serial writable continuity", gate.Name))
	}
	if current.owned && mode == apiv1.WorkspaceRepoReadOnly {
		problems = append(problems, fmt.Sprintf("workspace_revision_conflict: stage %q: repo-readonly cannot consume an established workspaceBranch", gate.Name))
	}
	return problems
}
