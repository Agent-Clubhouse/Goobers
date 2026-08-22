package v30

// repofrom.go is the WF022 core (dsl-3.0.md §4, delivery decisions 001/002 —
// binding): declared repo-handoff edges, checked as REACHING DEFINITIONS over
// the stage graph.
//
// A producer ("definition") is a stage that advances the run-branch ref as
// observed on the transport: an agentic stage on a writable repo workspace,
// the ref-advancing builtins (rebase-pr, update-behind-pr — the latter
// advances the ref provider-side, which counts), or a deterministic stage
// with the explicit commitsRepo opt-in. push-branch/push-remediated PUBLISH
// existing commits and are consumers only. Uncommitted worktree mutation is
// never a definition — the stage contract already guarantees it cannot cross
// a stage boundary in any mode.
//
// The computation is a forward fixed-point (gen/kill dataflow) over the
// compiled machine's edges — gate branches (fail edges included) and parallel
// fan-out, fan-in (branch-terminal @join resolved to the join state, as
// buildGraph resolves it), and mid-branch failure edges to the parallel's
// onFailure route are all real edges, and cycles converge at the fixed point. A
// consumer's requirement is the set of producers that can be the LAST
// producer before it on some forward path, excluding the consumer itself
// (gate-repass back-edges are attempt semantics, never repoFrom edges).
//
// It is deliberately NOT DFS back-edge pruning: on the live implementation
// shape (implement → review → local-ci → … → push-branch → ci-gate --fail→
// remediate-ci → review → local-ci) pruning misclassifies the loop
// re-entering through a DIFFERENT producer as droppable, yields [implement]
// alone at local-ci, and on every CI repass would fetch the head as of
// implement — silently discarding the remediation fix on exactly the path the
// lane exists to serve. The golden discriminator fixture pins this.
//
// Compile-half only (issue #3505 / §9 item 7): the actor-scoped runtime
// enforcement (recording the branch head around every non-producer repo
// stage; sanctioning the runner's own publish/recovery primitives per #3366)
// is the transport wave's work, not the interpreter's.

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// refAdvancingBuiltins are the built-in subcommands classified as producers
// by the dsl-3.0.md §4 commit reading: they advance the run-branch ref
// themselves (locally or provider-side) rather than publishing existing
// commits.
var refAdvancingBuiltins = map[string]bool{
	"rebase-pr":        true,
	"update-behind-pr": true,
}

// isRepoProducer reports whether a task advances the run-branch ref (a
// "definition" in the reaching-definitions sense).
func isRepoProducer(t apiv1.Task) bool {
	if t.CommitsRepo {
		return true
	}
	if t.Type == apiv1.TaskAgentic {
		return writesRepoWorkspace(t)
	}
	if t.Run != nil && len(t.Run.Command) >= 2 && t.Run.Command[0] == "goobers" {
		return refAdvancingBuiltins[t.Run.Command[1]]
	}
	return false
}

// isRepoConsumer reports whether a task consumes run-branch state: it
// executes on the writable repo workspace (workspace: repo, or the historical
// default). repo-readonly stages materialize the run's pinned BASE revision
// in detached head, never the run branch, so they are not consumers; scratch
// stages see no repository at all.
func isRepoConsumer(t apiv1.Task) bool {
	return writesRepoWorkspace(t)
}

// reachingProducers computes, for every state, the set of producers that can
// be the LAST producer before it on some forward path — the gen/kill forward
// fixed-point described in the file comment. ok is false on a structurally
// broken graph (those problems are reported elsewhere; a dataflow over a
// broken graph would only cascade noise — the CheckReachability pattern).
func reachingProducers(def Definition) (reaching map[string]map[string]bool, producers map[string]bool, ok bool) {
	m, buildProblems := newMachineForCheck(def)
	if len(buildProblems) > 0 || len(structuralProblems(m)) > 0 {
		return nil, nil, false
	}
	producers = make(map[string]bool, len(def.Spec.Tasks))
	for _, t := range def.Spec.Tasks {
		if isRepoProducer(t) {
			producers[t.Name] = true
		}
	}
	states := stateNames(def)
	reaching = make(map[string]map[string]bool, len(states))
	for _, name := range states {
		reaching[name] = map[string]bool{}
	}

	// Machine.Outgoing returns a branch-terminal stage's RAW target ("@join").
	// Resolve it to the owning parallel's join state with the same
	// graphTarget/graphJoinTargets resolution buildGraph applies, so parallel
	// fan-out/fan-in edges participate in the fixed-point: a producer inside a
	// branch (e.g. update-behind-pr on a scratch workspace — legal per
	// parallel rule 9, a producer per the §4 commit reading) must reach the
	// post-join consumer, or its correct declaration is refused as a dead
	// entry while the silent-loss under-declaration compiles clean.
	joinTargets := graphJoinTargets(def)
	// A branch failure hands the run to the parallel's onFailure route at ANY
	// point in any branch, so every branch state has a real runtime edge to it
	// — an already-ran producer in a sibling (or earlier in the failed branch
	// itself) can be the last producer the failure-lane consumer sees. The
	// parallel's own onFailure edge (Machine.Outgoing) only carries the
	// PRE-parallel reaching set; these implicit edges carry the in-branch
	// definitions.
	failureTargets := branchFailureTargets(m)

	changed := false
	flow := func(out map[string]bool, target string) {
		if !isStateName(target) {
			return
		}
		in, defined := reaching[target]
		if !defined {
			return
		}
		for producer := range out {
			if !in[producer] {
				in[producer] = true
				changed = true
			}
		}
	}
	for changed = true; changed; {
		changed = false
		for _, name := range states {
			// A producer's out-set is itself (it kills upstream definitions —
			// its commit is the new branch head); everything else passes its
			// in-set through, gates and parallels included.
			var out map[string]bool
			if producers[name] {
				out = map[string]bool{name: true}
			} else {
				out = reaching[name]
			}
			for _, target := range m.Outgoing(name) {
				flow(out, graphTarget(name, target, joinTargets))
			}
			if onFailure, ok := failureTargets[name]; ok {
				flow(out, onFailure)
			}
		}
	}
	return reaching, producers, true
}

// branchFailureTargets maps every state inside a parallel branch to that
// parallel's onFailure state, when one is declared and is a state name
// (@abort/@escalate end the run and consume nothing). These are the implicit
// mid-branch failure edges the dataflow needs: buildGraph draws only the
// control edge parallel→onFailure, but the definitions that can reach the
// failure lane include everything committed inside the branches before the
// failure.
func branchFailureTargets(m *Machine) map[string]string {
	byParallel := make(map[string]string, len(m.Def.Spec.Parallels))
	for _, p := range m.Def.Spec.Parallels {
		if p.OnFailure != "" && isStateName(p.OnFailure) {
			byParallel[p.Name] = p.OnFailure
		}
	}
	targets := map[string]string{}
	if len(byParallel) == 0 {
		return targets
	}
	for state, ref := range branchOwnership(m) {
		if onFailure, ok := byParallel[ref.parallel]; ok {
			targets[state] = onFailure
		}
	}
	return targets
}

// requiredCoverage is a consumer's repoFrom requirement: its reaching set
// minus itself (own prior attempts are attempt semantics).
func requiredCoverage(name string, reaching map[string]map[string]bool) map[string]bool {
	required := make(map[string]bool, len(reaching[name]))
	for producer := range reaching[name] {
		if producer != name {
			required[producer] = true
		}
	}
	return required
}

// repoHandoffProblems is WF022 (dsl-3.0.md §5): for every repo-consuming
// stage, the reaching-last-producer set must be exactly covered by its
// repoFrom declaration — an undeclared chain, an uncovered reaching producer,
// and a dead entry (a declared stage that can never immediately precede the
// consumer as its last producer) are each errors.
func repoHandoffProblems(def Definition) []string {
	reaching, producers, ok := reachingProducers(def)
	if !ok {
		return nil
	}
	tasks := make(map[string]bool, len(def.Spec.Tasks))
	for _, t := range def.Spec.Tasks {
		tasks[t.Name] = true
	}

	var problems []string
	for _, t := range def.Spec.Tasks {
		declared := []string(t.RepoFrom)
		if !isRepoConsumer(t) {
			if len(declared) > 0 {
				problems = append(problems, fmt.Sprintf(
					"task %q declares repoFrom but does not run on the writable repo workspace, so there is no repo state to hand off; remove repoFrom or make the stage workspace: repo",
					t.Name))
			}
			continue
		}

		required := requiredCoverage(t.Name, reaching)
		if len(required) > 0 && len(declared) == 0 {
			problems = append(problems, fmt.Sprintf(
				"task %q runs on the repo workspace after producer(s) %s but declares no repoFrom — the silent repo chain is inexpressible in DSL 3.0 (WF022, dsl-3.0.md §4): declare repoFrom: %s, or take the stage off the repo workspace",
				t.Name, quotedList(setKeys(required)), repoFromHint(setKeys(required))))
			continue
		}

		declaredSet := make(map[string]bool, len(declared))
		for _, d := range declared {
			declaredSet[d] = true
		}
		for _, producer := range setKeys(required) {
			if !declaredSet[producer] {
				problems = append(problems, fmt.Sprintf(
					"task %q repoFrom %s does not cover producer %q, which can be the last stage to advance the run branch before %q on some path; on that path the declared fetch would silently discard %q's commits — declare repoFrom: %s (WF022)",
					t.Name, quotedList(declared), producer, t.Name, producer, repoFromHint(setKeys(required))))
			}
		}
		for _, d := range declared {
			switch {
			case d == "":
				problems = append(problems, fmt.Sprintf("task %q repoFrom contains an empty stage name", t.Name))
			case d == t.Name:
				problems = append(problems, fmt.Sprintf(
					"task %q repoFrom names the stage itself; its own prior attempts are attempt semantics, never a handoff edge — remove %q from the list", t.Name, d))
			case !tasks[d]:
				problems = append(problems, fmt.Sprintf(
					"task %q repoFrom names %q, which is not a defined task (WF022)", t.Name, d))
			case !producers[d]:
				problems = append(problems, fmt.Sprintf(
					"task %q repoFrom names %q, which never advances the run branch (not an agentic repo stage, a ref-advancing builtin, or a commitsRepo stage) — a dead entry (WF022)", t.Name, d))
			case !required[d]:
				problems = append(problems, fmt.Sprintf(
					"task %q repoFrom names %q, but no forward path reaches %q with %q as its last producer — a dead entry (WF022)", t.Name, d, t.Name, d))
			}
		}
	}
	return problems
}

// RepoFromCoverage exposes the computed reaching-last-producer set per
// repo-consuming stage — the set repoFrom must cover — for the migrator
// (internal/dslmigrate --to 3.0, #3516) and tests. Keys are consumer task
// names; a consumer with no reaching producers is present with an empty
// slice. Structurally broken definitions return nil.
func RepoFromCoverage(def Definition) map[string][]string {
	reaching, _, ok := reachingProducers(def)
	if !ok {
		return nil
	}
	coverage := make(map[string][]string)
	for _, t := range def.Spec.Tasks {
		if !isRepoConsumer(t) {
			continue
		}
		coverage[t.Name] = setKeys(requiredCoverage(t.Name, reaching))
	}
	return coverage
}

func setKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

// repoFromHint renders the coverage set in the spelling an author would
// declare: scalar for one producer, list for fan-in.
func repoFromHint(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return "[" + strings.Join(names, ", ") + "]"
}
