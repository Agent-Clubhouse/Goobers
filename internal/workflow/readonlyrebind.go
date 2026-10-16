package workflow

import (
	"fmt"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// workspaceBranchOutput mirrors the runner's well-known stage output that
// rebinds the branch every later stage's workspace is provisioned on
// (internal/runner.WorkspaceBranchOutput, #392). It is restated here because
// the runner imports this package, not the other way round.
const workspaceBranchOutput = "workspaceBranch"

// workspaceRebindingBuiltins are the built-in `goobers` commands that emit
// the rebinding output: each binds every later stage to the selected pull
// request's head branch. A workflow need not also list the output under
// expectedOutputs for the rebinding to happen, so the command alone counts.
var workspaceRebindingBuiltins = []string{"gather-pr-context", "gather-sibling-context"}

// RebindsWorkspaceBranch reports whether t can rebind the run's workspace
// branch for the rest of the run. Only a deterministic task's
// rebinding output is honoured (an agentic stage's outputs are authored
// by the model), and it is recognised either from the task's declared
// expectedOutputs or from a built-in command known to emit it.
func RebindsWorkspaceBranch(t apiv1.Task) bool {
	if t.Type != apiv1.TaskDeterministic {
		return false
	}
	if slices.Contains(t.ExpectedOutputs, workspaceBranchOutput) {
		return true
	}
	if t.Run == nil || len(t.Run.Command) < 2 || t.Run.Command[0] != "goobers" {
		return false
	}
	return slices.Contains(workspaceRebindingBuiltins, t.Run.Command[1])
}

// ReadOnlyAfterRebind is a stage that declares workspace: repo-readonly but is
// reachable from a stage that can rebind the run's workspace branch. A
// rebound branch is sticky for the rest of the run and cannot back a
// detached, read-only checkout, so the stage cannot be provisioned on any run
// that took the rebind.
type ReadOnlyAfterRebind struct {
	// Stage is the repo-readonly task or agentic gate.
	Stage string
	// Kind is "task" or "gate".
	Kind string
	// Field is the declaration that selected repo-readonly, as an author
	// would write it.
	Field string
	// Rebinders are the rebinding tasks Stage is reachable from, in
	// declaration order.
	Rebinders []string
}

// Message renders the finding for workflow, naming the rebinding stage(s) and
// both ways to fix the ordering.
func (f ReadOnlyAfterRebind) Message(workflow string) string {
	return fmt.Sprintf(
		"workflow %q %s %q declares %s: repo-readonly but runs after %s, which can rebind the run's workspace branch (its %q output); a repo-readonly workspace cannot be created on a rebound branch, so %q fails to provision on every run that rebinds. Move %q before %s, or set %s: repo",
		workflow, f.Kind, f.Stage, f.Field, quotedStages(f.Rebinders), workspaceBranchOutput,
		f.Stage, f.Stage, quotedStages(f.Rebinders), f.Field,
	)
}

func quotedStages(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	if len(quoted) == 1 {
		return "stage " + quoted[0]
	}
	return "stages " + strings.Join(quoted, ", ")
}

// ReadOnlyWorkspacesAfterRebind reports every repo-readonly task or agentic
// gate in m that is reachable, through one or more transitions, from a task
// that can rebind the workspace branch (RebindsWorkspaceBranch). Rebinding
// cannot be undone later in a run (an empty value is a no-op, not a reset),
// so reachability alone decides it. Findings follow declaration order: tasks,
// then gates.
func ReadOnlyWorkspacesAfterRebind(m *Machine) []ReadOnlyAfterRebind {
	if m == nil {
		return nil
	}
	successors := machineSuccessors(m)
	reachedFrom := make(map[string][]string)
	for _, task := range m.Def.Spec.Tasks {
		if !RebindsWorkspaceBranch(task) {
			continue
		}
		for state := range reachableFrom(successors, task.Name) {
			reachedFrom[state] = append(reachedFrom[state], task.Name)
		}
	}
	var findings []ReadOnlyAfterRebind
	for _, task := range m.Def.Spec.Tasks {
		if task.EffectiveWorkspace() != apiv1.WorkspaceRepoReadOnly || len(reachedFrom[task.Name]) == 0 {
			continue
		}
		field := "workspace"
		if task.Run != nil && task.Run.Workspace != "" {
			field = "run.workspace"
		}
		findings = append(findings, ReadOnlyAfterRebind{Stage: task.Name, Kind: "task", Field: field, Rebinders: reachedFrom[task.Name]})
	}
	for _, gate := range m.Def.Spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.EffectiveWorkspace() != apiv1.WorkspaceRepoReadOnly || len(reachedFrom[gate.Name]) == 0 {
			continue
		}
		findings = append(findings, ReadOnlyAfterRebind{Stage: gate.Name, Kind: "gate", Field: "agentic.workspace", Rebinders: reachedFrom[gate.Name]})
	}
	return findings
}

// WorkspaceRebindersReaching returns the rebinding tasks (in declaration
// order) that stage is reachable from in m, for naming the culprit when a
// rebound branch refuses a read-only workspace at runtime.
func WorkspaceRebindersReaching(m *Machine, stage string) []string {
	if m == nil {
		return nil
	}
	successors := machineSuccessors(m)
	var out []string
	for _, task := range m.Def.Spec.Tasks {
		if !RebindsWorkspaceBranch(task) {
			continue
		}
		if _, ok := reachableFrom(successors, task.Name)[stage]; ok {
			out = append(out, task.Name)
		}
	}
	return out
}

// machineSuccessors merges the machine's transition targets with its graph
// edges. The graph resolves a parallel branch's "@join" to the concrete join
// state, and Outgoing contributes the parallel-to-join transition the graph
// leaves implicit; reserved targets name no state and are dropped.
func machineSuccessors(m *Machine) map[string][]string {
	successors := make(map[string][]string)
	add := func(from, to string) {
		if to == "" || IsReservedAnyTarget(to) || !m.Has(to) || slices.Contains(successors[from], to) {
			return
		}
		successors[from] = append(successors[from], to)
	}
	for _, edge := range m.Graph().Edges {
		add(edge.Source, edge.Target)
	}
	for _, node := range m.Graph().Nodes {
		for _, target := range m.Outgoing(node.ID) {
			add(node.ID, target)
		}
	}
	return successors
}

// reachableFrom returns every state reachable from start through at least one
// transition; start itself is included only when it lies on a cycle.
func reachableFrom(successors map[string][]string, start string) map[string]struct{} {
	seen := make(map[string]struct{})
	queue := append([]string(nil), successors[start]...)
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		if _, ok := seen[state]; ok {
			continue
		}
		seen[state] = struct{}{}
		queue = append(queue, successors[state]...)
	}
	return seen
}
