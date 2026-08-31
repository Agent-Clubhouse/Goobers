package v30

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

// maxParallelBranches bounds a single parallel's declared width. It is a
// WF-015-class runaway bound, deliberately host-declared rather than pinned in
// the schema so it can move without a DSL version bump.
const maxParallelBranches = 32

// parallelProblems implements the ten compile-time rules that make a static
// fan-out/fan-in definition safe to execute (design §5.5). A fan-out mistake
// must be caught here, never discovered at 2am in a live run (CFG-023).
//
// The rules exist to make branch attribution TOTAL: because branch subgraphs
// are provably disjoint, every event and artifact belongs to exactly one
// branch, which is what lets the journal, the completeness record, and the
// portal's transition projection avoid cross-branch phantom edges.
func parallelProblems(m *Machine) []string {
	def := m.Def
	if len(def.Spec.Parallels) == 0 {
		return nil
	}
	var problems []string

	// Which parallel owns each branch, and which states each branch body
	// contains. Computed once; rules 1-4 and 7-10 all read it.
	owner := branchOwnership(m)

	for _, parallel := range def.Spec.Parallels {
		problems = append(problems, parallelShapeProblems(m, parallel)...)
		problems = append(problems, parallelBodyProblems(m, parallel, owner)...)
	}

	problems = append(problems, joinEntryProblems(m)...)
	problems = append(problems, strayJoinProblems(m, owner)...)
	problems = append(problems, branchInputsFromProblems(m)...)

	sort.Strings(problems)
	return problems
}

type branchInputReference struct {
	parallel  string
	branch    string
	stage     string
	key       string
	shorthand bool
}

func splitBranchInputReference(value string) (branchInputReference, bool) {
	parts := strings.SplitN(value, ".", 4)
	switch len(parts) {
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return branchInputReference{}, false
		}
		return branchInputReference{
			parallel:  parts[0],
			branch:    parts[1],
			key:       parts[2],
			shorthand: true,
		}, true
	case 4:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
			return branchInputReference{}, false
		}
		return branchInputReference{
			parallel: parts[0],
			branch:   parts[1],
			stage:    parts[2],
			key:      parts[3],
		}, true
	default:
		return branchInputReference{}, false
	}
}

func branchInputsFromProblems(m *Machine) []string {
	joins := make(map[string]apiv1.Parallel, len(m.Def.Spec.Parallels))
	for _, parallel := range m.Def.Spec.Parallels {
		joins[parallel.Join] = parallel
	}

	var problems []string
	for _, task := range m.Def.Spec.Tasks {
		join, isJoin := joins[task.Name]
		for _, inputKey := range sortedKeys(task.InputsFrom) {
			value := task.InputsFrom[inputKey]
			ref, parsed := splitBranchInputReference(value)
			_, knownParallel := m.Parallel(ref.parallel)
			if parsed {
				if _, stageQualified := m.Task(ref.parallel); stageQualified {
					continue
				}
			}
			if !isJoin && (!parsed || !knownParallel) {
				continue
			}
			if !parsed {
				if isJoin && strings.Count(value, ".") >= 2 {
					problems = append(problems, fmt.Sprintf(
						"task %q inputsFrom %q has malformed branch-qualified reference %q; use <parallel>.<branch>.<stage>.<outputKey>",
						task.Name, inputKey, value))
				}
				continue
			}
			if !knownParallel {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q references unknown parallel %q",
					task.Name, inputKey, ref.parallel))
				continue
			}
			if !isJoin {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q uses branch-qualified reference %q but is not a parallel join",
					task.Name, inputKey, value))
				continue
			}
			if ref.parallel != join.Name {
				problems = append(problems, fmt.Sprintf(
					"task %q is the join of parallel %q but inputsFrom %q references parallel %q",
					task.Name, join.Name, inputKey, ref.parallel))
				continue
			}

			var branch apiv1.Branch
			foundBranch := false
			for _, candidate := range join.Branches {
				if candidate.Name == ref.branch {
					branch = candidate
					foundBranch = true
					break
				}
			}
			if !foundBranch {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q references unknown branch %q of parallel %q",
					task.Name, inputKey, ref.branch, ref.parallel))
				continue
			}

			if ref.shorthand {
				terminals := joinTerminalStates(m, branch.Start)
				if len(terminals) != 1 {
					problems = append(problems, fmt.Sprintf(
						"parallel %q branch has %d join-terminal stages, qualify the stage (branch %q, task %q inputsFrom %q)",
						join.Name, len(terminals), branch.Name, task.Name, inputKey))
					continue
				}
				ref.stage = terminals[0]
			}

			inBranch := false
			for _, state := range branchBody(m, branch.Start) {
				if state == ref.stage {
					inBranch = true
					break
				}
			}
			if !inBranch {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q references unknown stage %q in parallel %q branch %q",
					task.Name, inputKey, ref.stage, ref.parallel, ref.branch))
				continue
			}
			producer, isTask := m.Task(ref.stage)
			if !isTask {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q references stage %q in parallel %q branch %q, but it is not a task and produces no outputs",
					task.Name, inputKey, ref.stage, ref.parallel, ref.branch))
				continue
			}
			if !precedesBranchJoinOnEveryPath(m, branch.Start, ref.stage) {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q references stage %q in parallel %q branch %q, but that stage does not run on every successful path to @join",
					task.Name, inputKey, ref.stage, ref.parallel, ref.branch))
			}
			if len(producer.ExpectedOutputs) > 0 && !containsString(producer.ExpectedOutputs, ref.key) {
				problems = append(problems, fmt.Sprintf(
					"task %q inputsFrom %q references output %q from parallel %q branch %q stage %q, but that stage declares outputs %v",
					task.Name, inputKey, ref.key, ref.parallel, ref.branch, ref.stage, producer.ExpectedOutputs))
			}
		}
	}
	return problems
}

func precedesBranchJoinOnEveryPath(m *Machine, start, producer string) bool {
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
		if state == TerminalComplete || model.IsReservedAnyTarget(state) || !m.Has(state) {
			continue
		}
		seen[state] = true
		stack = append(stack, m.Outgoing(state)...)
	}
	return true
}

type stateGraph interface {
	Has(string) bool
	Outgoing(string) []string
}

func joinTerminalStates(m stateGraph, start string) []string {
	var terminals []string
	for _, state := range branchBody(m, start) {
		for _, target := range m.Outgoing(state) {
			if target == TargetJoin {
				terminals = append(terminals, state)
				break
			}
		}
	}
	sort.Strings(terminals)
	return terminals
}

// branchRef names the parallel and branch a state belongs to.
type branchRef struct {
	parallel string
	branch   string
}

// branchOwnership maps each state reachable from a branch start to the branch
// that owns it. A state reachable from two branches is reported by rule 1 and
// arbitrarily attributed to the first, so downstream rules stay deterministic.
func branchOwnership(m *Machine) map[string]branchRef {
	owner := make(map[string]branchRef)
	for _, parallel := range m.Def.Spec.Parallels {
		for _, branch := range parallel.Branches {
			ref := branchRef{parallel: parallel.Name, branch: branch.Name}
			for _, state := range branchBody(m, branch.Start) {
				if _, taken := owner[state]; !taken {
					owner[state] = ref
				}
			}
		}
	}
	return owner
}

// branchBody returns every declared state reachable from start without leaving
// through a reserved target. Reserved targets (@join/@abort/@escalate) and the
// empty completion target stop the walk, so a branch body never bleeds into the
// join or a failure route.
func branchBody(m stateGraph, start string) []string {
	seen := map[string]bool{}
	var out []string
	stack := []string{start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == TerminalComplete || model.IsReservedAnyTarget(state) || seen[state] {
			continue
		}
		if !m.Has(state) {
			continue
		}
		seen[state] = true
		out = append(out, state)
		stack = append(stack, m.Outgoing(state)...)
	}
	sort.Strings(out)
	return out
}

// parallelShapeProblems covers the rules that read only the parallel's own
// declaration: bounds (5), policy completeness (6), and timeout coherence (8).
func parallelShapeProblems(m *Machine, p apiv1.Parallel) []string {
	var problems []string

	// Rule 5 — bounds.
	if len(p.Branches) < 2 {
		problems = append(problems, fmt.Sprintf(
			"parallel %q declares %d branches; a parallel needs at least 2 (use an ordinary task for one)",
			p.Name, len(p.Branches)))
	}
	if len(p.Branches) > maxParallelBranches {
		problems = append(problems, fmt.Sprintf(
			"parallel %q declares %d branches; the maximum is %d",
			p.Name, len(p.Branches), maxParallelBranches))
	}
	seenBranch := map[string]bool{}
	for _, branch := range p.Branches {
		if branch.Name == "" {
			problems = append(problems, fmt.Sprintf("parallel %q has a branch with no name", p.Name))
			continue
		}
		if seenBranch[branch.Name] {
			problems = append(problems, fmt.Sprintf("parallel %q declares duplicate branch %q", p.Name, branch.Name))
		}
		seenBranch[branch.Name] = true
		if branch.Start == "" {
			problems = append(problems, fmt.Sprintf("parallel %q branch %q has no start state", p.Name, branch.Name))
		} else if !m.Has(branch.Start) {
			problems = append(problems, fmt.Sprintf(
				"parallel %q branch %q start %q is not a defined state", p.Name, branch.Name, branch.Start))
		}
	}
	if p.MaxConcurrentBranches < 0 {
		problems = append(problems, fmt.Sprintf(
			"parallel %q maxConcurrentBranches %d is negative", p.Name, p.MaxConcurrentBranches))
	}

	// Rule 6 — policy completeness. onFailure is required for the cancelling
	// policies and forbidden under continue_on_error, where the join always
	// runs and owns the decision; declaring both would name two contradictory
	// owners of the same failure.
	switch p.FailurePolicy {
	case apiv1.BranchFailFast, apiv1.BranchAllOrNothing:
		if p.OnFailure == "" {
			problems = append(problems, fmt.Sprintf(
				"parallel %q failurePolicy %q requires onFailure (a state name, @abort, or @escalate) so branch failure is a defined branch, never a silent stop",
				p.Name, p.FailurePolicy))
		} else if isStateName(p.OnFailure) && !m.Has(p.OnFailure) {
			problems = append(problems, fmt.Sprintf(
				"parallel %q onFailure %q is not a defined state", p.Name, p.OnFailure))
		} else if model.IsReservedBranchTarget(p.OnFailure) {
			problems = append(problems, fmt.Sprintf(
				"parallel %q onFailure must not be %q", p.Name, TargetJoin))
		}
	case apiv1.BranchContinueOnError:
		if p.OnFailure != "" {
			problems = append(problems, fmt.Sprintf(
				"parallel %q failurePolicy %q must not declare onFailure; the join always runs and owns the decision via the completeness record",
				p.Name, p.FailurePolicy))
		}
	case "":
		problems = append(problems, fmt.Sprintf(
			"parallel %q has no failurePolicy; declare one of fail_fast, all_or_nothing, continue_on_error (there is no default)",
			p.Name))
	default:
		problems = append(problems, fmt.Sprintf(
			"parallel %q failurePolicy %q is not one of fail_fast, all_or_nothing, continue_on_error",
			p.Name, p.FailurePolicy))
	}

	if p.BranchTimeoutSeconds < 0 {
		problems = append(problems, fmt.Sprintf(
			"parallel %q branchTimeoutSeconds %d is negative", p.Name, p.BranchTimeoutSeconds))
	}

	// Rule 8 — timeout coherence. A single stage must not be able to guarantee
	// its own branch times out. This is a NEW check: CheckStageTimeoutCoherence
	// compares a bounded-wait poll interval against its own stage's timeout and
	// has no notion of a budget above the stage.
	if p.BranchTimeoutSeconds > 0 {
		for _, branch := range p.Branches {
			for _, state := range branchBody(m, branch.Start) {
				task, ok := m.Task(state)
				if !ok || task.TimeoutSeconds <= 0 {
					continue
				}
				if task.TimeoutSeconds > p.BranchTimeoutSeconds {
					problems = append(problems, fmt.Sprintf(
						"parallel %q branch %q: task %q timeoutSeconds %d exceeds branchTimeoutSeconds %d, so the branch can never finish within its budget",
						p.Name, branch.Name, task.Name, task.TimeoutSeconds, p.BranchTimeoutSeconds))
				}
			}
		}
	}

	return problems
}

// parallelBodyProblems covers the rules that read the branch subgraphs:
// disjointness (1), branch-terminal exits (2), no nesting (7), and the
// workspace restriction (9) and human-gate restriction (10).
func parallelBodyProblems(m *Machine, p apiv1.Parallel, owner map[string]branchRef) []string {
	var problems []string

	bodies := make(map[string][]string, len(p.Branches))
	for _, branch := range p.Branches {
		if branch.Start == "" || !m.Has(branch.Start) {
			continue
		}
		bodies[branch.Name] = branchBody(m, branch.Start)
	}

	// Rule 1 — disjointness. A state shared between two branches, or shared
	// with the join / the pre-parallel root path / the onFailure route, makes
	// branch attribution ambiguous.
	for _, branch := range p.Branches {
		for _, state := range bodies[branch.Name] {
			if ref, ok := owner[state]; ok && (ref.parallel != p.Name || ref.branch != branch.Name) {
				problems = append(problems, fmt.Sprintf(
					"parallel %q: state %q is reachable from both branch %q and branch %q (of parallel %q); branch subgraphs must be disjoint so every event belongs to exactly one branch",
					p.Name, state, branch.Name, ref.branch, ref.parallel))
			}
			if state == p.Join {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch %q reaches the join state %q directly; a branch must end at %q instead",
					p.Name, branch.Name, p.Join, TargetJoin))
			}
			if p.OnFailure != "" && state == p.OnFailure {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch %q reaches its own onFailure state %q; the failure route must be outside every branch",
					p.Name, branch.Name, p.OnFailure))
			}
			if state == p.Name {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch %q routes back to the parallel itself", p.Name, branch.Name))
			}

			// Rule 7 — no nesting. One level only: this removes the
			// state-model and cancellation-tree complexity for a use case we
			// do not have.
			if _, nested := m.Parallel(state); nested {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch %q contains parallel %q; nested parallels are not supported",
					p.Name, branch.Name, state))
			}

			problems = append(problems, branchStateProblems(m, p, branch, state)...)
		}
	}

	// Rule 2 — branch exits are branch-terminal. Stated as the DECIDABLE form:
	// every branch state can REACH @join/@abort/@escalate. Like the existing
	// canExit fixed point this is reachability, not termination — a back-edge
	// cycle that never actually exits still satisfies it, and is bounded at
	// runtime by branchTimeoutSeconds.
	for _, branch := range p.Branches {
		body := bodies[branch.Name]
		if len(body) == 0 {
			continue
		}
		exits := branchExitSet(m, body)
		for _, state := range body {
			if !exits[state] {
				problems = append(problems, fmt.Sprintf(
					"parallel %q branch %q: state %q cannot reach %q, @abort, or @escalate (a branch may not complete the run)",
					p.Name, branch.Name, state, TargetJoin))
			}
		}
	}

	return problems
}

// branchExitSet is the fixed point of "can reach a branch-terminal target".
// Completing the run (the empty target) is deliberately NOT an exit: a branch
// must never terminate the whole run by falling off its end.
func branchExitSet(m *Machine, body []string) map[string]bool {
	inBody := make(map[string]bool, len(body))
	for _, state := range body {
		inBody[state] = true
	}
	exits := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, state := range body {
			if exits[state] {
				continue
			}
			for _, target := range m.Outgoing(state) {
				if model.IsReservedBranchTarget(target) || model.IsReservedTarget(target) || (inBody[target] && exits[target]) {
					exits[state] = true
					changed = true
					break
				}
			}
		}
	}
	return exits
}

// branchStateProblems covers rules 9 and 10, which restrict what a branch stage
// may be. Both exist because the runtime cannot honour the alternative today.
func branchStateProblems(m *Machine, p apiv1.Parallel, branch apiv1.Branch, state string) []string {
	var problems []string

	// Rule 10 — no human gate inside a branch. A human gate pauses the run and
	// returns from the walk, and resume restores it by overriding the single
	// start cursor. "One branch waits on a human for three days while its
	// siblings hold workspaces" is a new suspension model, not a
	// generalisation of the existing one.
	if gate, ok := m.Gate(state); ok && gate.Evaluator == apiv1.EvaluatorHuman {
		problems = append(problems, fmt.Sprintf(
			"parallel %q branch %q: gate %q is a human gate; human gates are not supported inside a branch (put it before the parallel or at the join)",
			p.Name, branch.Name, state))
	}

	// Rule 9 — no writable repo workspace inside a branch. Every stage worktree
	// is otherwise created on ONE run branch, and git refuses to check one
	// branch out in two worktrees, so concurrent repo-backed branch stages
	// collide outright.
	//
	// Checks the EFFECTIVE workspace — including the unset default, which
	// resolves to a writable repo worktree for both a deterministic task
	// (Run.Workspace) and an agentic task/gate (Task.Workspace /
	// Gate.Agentic.Workspace) — not just an explicit `workspace: repo`.
	// Mirrors internal/runner's taskWorkspaceMode/gateWorkspaceMode
	// resolution exactly; duplicated here rather than imported because
	// this package compiles before runner and importing it would cycle back
	// (runner -> workflow -> this package).
	if task, ok := m.Task(state); ok && branchEffectiveTaskWorkspace(task) == apiv1.WorkspaceRepo {
		problems = append(problems, fmt.Sprintf(
			"parallel %q branch %q: task %q resolves to a writable repo workspace; branch stages must use scratch or repo-readonly (concurrent repo-backed branches collide on the run branch)",
			p.Name, branch.Name, state))
	}
	if gate, ok := m.Gate(state); ok && gate.Evaluator == apiv1.EvaluatorAgentic && branchEffectiveGateWorkspace(gate) == apiv1.WorkspaceRepo {
		problems = append(problems, fmt.Sprintf(
			"parallel %q branch %q: gate %q resolves to a writable repo workspace; branch stages must use scratch or repo-readonly (concurrent repo-backed branches collide on the run branch)",
			p.Name, branch.Name, state))
	}

	return problems
}

// branchEffectiveTaskWorkspace resolves the workspace a task actually runs in.
// Run.Workspace is authoritative for a deterministic task; Task.Workspace is
// the seam an agentic task uses instead; unset means the writable repo
// worktree either way (internal/runner.taskWorkspaceMode's default).
func branchEffectiveTaskWorkspace(t apiv1.Task) apiv1.WorkspaceMode {
	if t.Run != nil && t.Run.Workspace != "" {
		return t.Run.Workspace
	}
	if t.Workspace != "" {
		return t.Workspace
	}
	return apiv1.WorkspaceRepo
}

// branchEffectiveGateWorkspace resolves the workspace an agentic gate
// evaluates in, mirroring internal/runner.gateWorkspaceMode.
func branchEffectiveGateWorkspace(g apiv1.Gate) apiv1.WorkspaceMode {
	if g.Agentic != nil && g.Agentic.Workspace != "" {
		return g.Agentic.Workspace
	}
	return apiv1.WorkspaceRepo
}

// joinEntryProblems covers rule 3: the join is parallel-entered only. If any
// ordinary edge targets a join state, the join could run without its branches
// having settled.
func joinEntryProblems(m *Machine) []string {
	joins := make(map[string]string, len(m.Def.Spec.Parallels))
	for _, p := range m.Def.Spec.Parallels {
		if p.Join != "" {
			joins[p.Join] = p.Name
		}
	}
	if len(joins) == 0 {
		return nil
	}

	var problems []string
	for _, p := range m.Def.Spec.Parallels {
		if p.Join == "" {
			problems = append(problems, fmt.Sprintf("parallel %q has no join state", p.Name))
			continue
		}
		if !m.Has(p.Join) {
			problems = append(problems, fmt.Sprintf("parallel %q join %q is not a defined state", p.Name, p.Join))
		}
		if _, isParallel := m.Parallel(p.Join); isParallel {
			problems = append(problems, fmt.Sprintf("parallel %q join %q is itself a parallel", p.Name, p.Join))
		}
	}

	for _, task := range m.Def.Spec.Tasks {
		if ownerName, ok := joins[task.Next]; ok {
			problems = append(problems, fmt.Sprintf(
				"task %q routes to %q, which is the join of parallel %q; a join may only be entered through its parallel",
				task.Name, task.Next, ownerName))
		}
	}
	for _, gate := range m.Def.Spec.Gates {
		for _, outcome := range sortedKeys(gate.Branches) {
			if ownerName, ok := joins[gate.Branches[outcome]]; ok {
				problems = append(problems, fmt.Sprintf(
					"gate %q branch %q routes to %q, which is the join of parallel %q; a join may only be entered through its parallel",
					gate.Name, outcome, gate.Branches[outcome], ownerName))
			}
		}
	}
	if ownerName, ok := joins[m.Def.Spec.Start]; ok {
		problems = append(problems, fmt.Sprintf(
			"start state %q is the join of parallel %q; a join may only be entered through its parallel",
			m.Def.Spec.Start, ownerName))
	}
	return problems
}

// strayJoinProblems covers rule 4: @join is branch-scoped. Using it outside any
// branch has no join to route to, and would otherwise silently satisfy the
// canExit fixed point.
func strayJoinProblems(m *Machine, owner map[string]branchRef) []string {
	var problems []string
	for _, task := range m.Def.Spec.Tasks {
		if !model.IsReservedBranchTarget(task.Next) {
			continue
		}
		if _, inBranch := owner[task.Name]; !inBranch {
			problems = append(problems, fmt.Sprintf(
				"task %q routes to %q but is not inside a parallel branch", task.Name, TargetJoin))
		}
	}
	for _, gate := range m.Def.Spec.Gates {
		for _, outcome := range sortedKeys(gate.Branches) {
			if !model.IsReservedBranchTarget(gate.Branches[outcome]) {
				continue
			}
			if _, inBranch := owner[gate.Name]; !inBranch {
				problems = append(problems, fmt.Sprintf(
					"gate %q branch %q routes to %q but the gate is not inside a parallel branch",
					gate.Name, outcome, TargetJoin))
			}
		}
	}
	return problems
}
