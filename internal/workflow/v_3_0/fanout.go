package v30

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

// The DSL 3.1 fan-out graph rules (docs/design/plan-driven-dynamic-fan-out.md
// §10): the selective-parallel optional/selection pairing and every map rule
// that reads only the state machine. Source-slot rules (JSON slot, maxSize,
// dominance) need the artifact contract index and live in the version router.
// Only DSL 3.1 admits these fields — the router refuses them for every older
// pin — so a 3.0 definition never reaches a rule here.

// maxMapConcurrency is the host bound on Map.MaxConcurrent: a map's width is
// bounded by the static parallel cap, so its concurrency is too.
const maxMapConcurrency = maxParallelBranches

var mapBindingNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// selectionProblems pairs optional branches with a selection source: each is
// meaningless without the other.
func selectionProblems(p apiv1.Parallel) []string {
	optional := 0
	for _, branch := range p.Branches {
		if branch.Optional {
			optional++
		}
	}
	switch {
	case p.Selection != nil && optional == 0:
		return []string{fmt.Sprintf(
			"parallel %q declares selection but no optional branch; mark the branches the selection may skip optional: true",
			p.Name)}
	case p.Selection == nil && optional > 0:
		return []string{fmt.Sprintf(
			"parallel %q declares %d optional branch(es) but no selection; an optional branch runs only when a selection names it",
			p.Name, optional)}
	}
	return nil
}

// mapOwnership maps each mapped task to the first map that names it, so the
// stray-@join and join-entry rules treat a mapped task like a branch body.
func mapOwnership(m *Machine) map[string]string {
	owner := make(map[string]string, len(m.Def.Spec.Maps))
	for _, fanOut := range m.Def.Spec.Maps {
		if _, taken := owner[fanOut.Task]; !taken && fanOut.Task != "" {
			owner[fanOut.Task] = fanOut.Name
		}
	}
	return owner
}

// mapProblems applies every graph-level map rule.
func mapProblems(m *Machine, branchOwner map[string]branchRef) []string {
	var problems []string
	taskOwner := mapOwnership(m)
	for _, fanOut := range m.Def.Spec.Maps {
		problems = append(problems, mapShapeProblems(m, fanOut)...)
		problems = append(problems, mapTaskProblems(m, fanOut, taskOwner, branchOwner)...)
		problems = append(problems, mapBindingProblems(m, fanOut)...)
	}
	return problems
}

// mapShapeProblems covers the map's own declaration: name, policy and failure
// route, join, and the timeout and concurrency bounds.
func mapShapeProblems(m *Machine, fanOut apiv1.Map) []string {
	var problems []string
	if strings.Contains(fanOut.Name, ".") {
		problems = append(problems, fmt.Sprintf(
			"map %q name must not contain '.'; dots are reserved for qualified references", fanOut.Name))
	}
	problems = append(problems, mapPolicyProblems(m, fanOut)...)
	switch {
	case fanOut.Join == "":
		problems = append(problems, fmt.Sprintf("map %q has no join state", fanOut.Name))
	case !m.Has(fanOut.Join):
		problems = append(problems, fmt.Sprintf("map %q join %q is not a defined state", fanOut.Name, fanOut.Join))
	case isFanOutState(m, fanOut.Join):
		problems = append(problems, fmt.Sprintf("map %q join %q is itself a parallel or map", fanOut.Name, fanOut.Join))
	case fanOut.Join == fanOut.Task:
		problems = append(problems, fmt.Sprintf("map %q join %q is its own mapped task", fanOut.Name, fanOut.Join))
	}
	if fanOut.ItemTimeoutSeconds < 0 {
		problems = append(problems, fmt.Sprintf(
			"map %q itemTimeoutSeconds %d is negative", fanOut.Name, fanOut.ItemTimeoutSeconds))
	}
	if fanOut.MaxConcurrent < 0 || fanOut.MaxConcurrent > maxMapConcurrency {
		problems = append(problems, fmt.Sprintf(
			"map %q maxConcurrent %d is outside 1..%d (omit it for sequential execution)",
			fanOut.Name, fanOut.MaxConcurrent, maxMapConcurrency))
	}
	return problems
}

// mapPolicyProblems mirrors static parallel rule 6: onFailure is required for
// the cancelling policies and forbidden under continue_on_error.
func mapPolicyProblems(m *Machine, fanOut apiv1.Map) []string {
	switch fanOut.FailurePolicy {
	case apiv1.BranchFailFast, apiv1.BranchAllOrNothing:
		switch {
		case fanOut.OnFailure == "":
			return []string{fmt.Sprintf(
				"map %q failurePolicy %q requires onFailure (a state name, @abort, or @escalate) so item failure is a defined branch, never a silent stop",
				fanOut.Name, fanOut.FailurePolicy)}
		case model.IsReservedBranchTarget(fanOut.OnFailure):
			return []string{fmt.Sprintf("map %q onFailure must not be %q", fanOut.Name, TargetJoin)}
		case isStateName(fanOut.OnFailure) && !m.Has(fanOut.OnFailure):
			return []string{fmt.Sprintf("map %q onFailure %q is not a defined state", fanOut.Name, fanOut.OnFailure)}
		case fanOut.OnFailure == fanOut.Task:
			return []string{fmt.Sprintf("map %q onFailure %q is its own mapped task", fanOut.Name, fanOut.OnFailure)}
		}
	case apiv1.BranchContinueOnError:
		if fanOut.OnFailure != "" {
			return []string{fmt.Sprintf(
				"map %q failurePolicy %q must not declare onFailure; the join always runs and owns the decision via the completeness record",
				fanOut.Name, fanOut.FailurePolicy)}
		}
	case "":
		return []string{fmt.Sprintf(
			"map %q has no failurePolicy; declare one of fail_fast, all_or_nothing, continue_on_error (there is no default)",
			fanOut.Name)}
	default:
		return []string{fmt.Sprintf(
			"map %q failurePolicy %q is not one of fail_fast, all_or_nothing, continue_on_error",
			fanOut.Name, fanOut.FailurePolicy)}
	}
	return nil
}

// mapTaskProblems covers the mapped task: it exists and is a task, only its
// own map enters it, it ends at a branch-terminal target, its workspace is
// not writable, it enables no child workflows, and its timeout fits the item
// budget.
func mapTaskProblems(m *Machine, fanOut apiv1.Map, taskOwner map[string]string, branchOwner map[string]branchRef) []string {
	task, ok := m.Task(fanOut.Task)
	if !ok {
		if isFanOutState(m, fanOut.Task) {
			return []string{fmt.Sprintf(
				"map %q task %q is a parallel or map; a map runs one task and nested fan-out is not supported",
				fanOut.Name, fanOut.Task)}
		}
		return []string{fmt.Sprintf("map %q task %q is not a defined task", fanOut.Name, fanOut.Task)}
	}
	var problems []string
	if owner := taskOwner[task.Name]; owner != fanOut.Name {
		problems = append(problems, fmt.Sprintf(
			"map %q task %q is already owned by map %q; a mapped task belongs to exactly one map",
			fanOut.Name, task.Name, owner))
	}
	if ref, inBranch := branchOwner[task.Name]; inBranch {
		problems = append(problems, fmt.Sprintf(
			"map %q task %q is also reachable from parallel %q branch %q; a mapped task belongs to exactly one fan-out",
			fanOut.Name, task.Name, ref.parallel, ref.branch))
	}
	for _, source := range mapTaskEntrySources(m, fanOut) {
		problems = append(problems, fmt.Sprintf(
			"map %q task %q is entered by %q; only its map may enter a mapped task",
			fanOut.Name, task.Name, source))
	}
	if task.Next != TargetJoin && !model.IsReservedTarget(task.Next) {
		problems = append(problems, fmt.Sprintf(
			"map %q task %q routes to %q; a mapped task must end at %q, @abort, or @escalate",
			fanOut.Name, task.Name, displayTarget(task.Next), TargetJoin))
	}
	if branchEffectiveTaskWorkspace(task) == apiv1.WorkspaceRepo {
		problems = append(problems, fmt.Sprintf(
			"map %q task %q resolves to a writable repo workspace; mapped tasks must use scratch or repo-readonly, even at maxConcurrent 1",
			fanOut.Name, task.Name))
	}
	if task.ChildWorkflows != nil {
		problems = append(problems, fmt.Sprintf(
			"map %q task %q declares childWorkflows; a mapped task may not enable child tools",
			fanOut.Name, task.Name))
	}
	if fanOut.ItemTimeoutSeconds > 0 && task.TimeoutSeconds > fanOut.ItemTimeoutSeconds {
		problems = append(problems, fmt.Sprintf(
			"map %q: task %q timeoutSeconds %d exceeds itemTimeoutSeconds %d, so an item can never finish within its budget",
			fanOut.Name, task.Name, task.TimeoutSeconds, fanOut.ItemTimeoutSeconds))
	}
	return problems
}

// mapTaskEntrySources lists every state, other than the owning map's own
// fan-out edge, whose transitions can enter the mapped task.
func mapTaskEntrySources(m *Machine, fanOut apiv1.Map) []string {
	var sources []string
	if m.Def.Spec.Start == fanOut.Task {
		sources = append(sources, "spec.start")
	}
	for _, state := range stateNames(m.Def) {
		targets := m.Outgoing(state)
		if state == fanOut.Name {
			targets = targets[1:]
		}
		for _, target := range targets {
			if target == fanOut.Task {
				sources = append(sources, state)
				break
			}
		}
	}
	sort.Strings(sources)
	return sources
}

// mapBindingProblems keeps bindings a closed, collision-free set of dynamic
// invocation inputs.
func mapBindingProblems(m *Machine, fanOut apiv1.Map) []string {
	task, _ := m.Task(fanOut.Task)
	var problems []string
	for _, name := range sortedKeys(fanOut.Bindings) {
		value := fanOut.Bindings[name]
		if !mapBindingNamePattern.MatchString(name) {
			problems = append(problems, fmt.Sprintf(
				"map %q binding %q is not a valid input name (letters, digits, '_' or '-', at most 128)", fanOut.Name, name))
		}
		switch value {
		case apiv1.MapBindingItem, apiv1.MapBindingIdentity, apiv1.MapBindingDigest:
		default:
			problems = append(problems, fmt.Sprintf(
				"map %q binding %q value %q is not one of item, identity, digest", fanOut.Name, name, value))
		}
		if _, static := task.Inputs[name]; static {
			problems = append(problems, fmt.Sprintf(
				"map %q binding %q collides with static input %q of task %q", fanOut.Name, name, name, task.Name))
		}
		if _, static := task.InputsFrom[name]; static {
			problems = append(problems, fmt.Sprintf(
				"map %q binding %q collides with inputsFrom input %q of task %q", fanOut.Name, name, name, task.Name))
		}
	}
	return problems
}

func isFanOutState(m *Machine, name string) bool {
	if _, ok := m.Parallel(name); ok {
		return true
	}
	_, ok := m.Map(name)
	return ok
}

func displayTarget(target string) string {
	if target == TerminalComplete {
		return "<complete>"
	}
	return target
}
