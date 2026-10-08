package workflow

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// siblingOverlapOutputKey is the deterministic overlap signal
// gather-sibling-context emits ("true" when the selected PR's files overlap
// another open managed PR).
const siblingOverlapOutputKey = "hasSiblingOverlap"

// sequencingBuiltins are the goobers built-in stages that durably record
// sequencing or remediation state for a selected PR: a lander election, a
// published verdict that parks the PR blocked-on-sibling or
// needs-remediation, a landing, a recorded merge refusal/demotion, an
// escalation, or a branch update.
var sequencingBuiltins = []string{
	"apply-verdict",
	"elect-lander",
	"merge-pr",
	"rebase-pr",
	"record-merge-refusal",
	"remediation-checkpoint",
	"update-behind-pr",
}

// sequencingPolicyActions are declared policy actions with the same durable
// effect, so a custom stage that declares one counts as sequencing.
var sequencingPolicyActions = []string{
	"demote-pr",
	"escalate-pr",
	"merge-pr",
	"push-pr-branch",
	"rebase-pr",
	"record-merge-refusal",
	"route-provider-verdict",
	"update-pr-branch",
}

// CheckSiblingOverlapSequencing reports gates that route a detected sibling
// overlap (hasSiblingOverlap=true) to run termination without any path through
// a stage that durably records sequencing or remediation state (#5592).
//
// Lazy remediation deliberately counts only strong markers (needs-remediation,
// failing CI) and crowned or solitary behind-base landers, so two green,
// conflicting, unlabeled managed PRs produce zero remediation demand on their
// own. Lander election and verdict publication are what turn such a cluster
// into a crown or a parked sibling. A graph that ends the overlap branch
// before reaching them makes the selector pick and silently terminate on the
// same PR on every tick while remediation stays idle forever.
//
// The analysis is structural: every normally reachable path from the overlap
// outcome must reach a sequencing stage or become operator-visible
// ("@escalate"). A single silent path (completion or "@abort" before any
// sequencing stage) is reported even when sibling gate outcomes do sequence,
// because the selector can take that path on every tick. Runner-forced
// escalation branches are not normal routes and are ignored; an unresolvable
// target is reported elsewhere and treated as not provably silent.
func CheckSiblingOverlapSequencing(def Definition) []string {
	g := newOverlapGraph(def.Spec)
	var problems []string
	for _, gate := range def.Spec.Gates {
		outcome, ok := siblingOverlapOutcome(gate)
		if !ok {
			continue
		}
		target, ok := gate.Branches[outcome]
		if !ok {
			continue
		}
		if g.alwaysSequences(target) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"workflow %q gate %q routes %s=true (outcome %q -> %s) to run termination without reaching a stage that records sequencing or remediation state (goobers elect-lander, goobers apply-verdict, or another stage declaring %s); green overlapping managed PRs then produce no remediation demand, so the run selects and terminates on the same PR indefinitely. Route the overlap outcome through elect-lander and apply-verdict, as the reference merge-review workflow does",
			def.Name, gate.Name, siblingOverlapOutputKey, outcome, describeTarget(target),
			strings.Join(sequencingPolicyActions, "/"),
		))
	}
	return problems
}

// siblingOverlapOutcome returns the gate outcome taken when
// hasSiblingOverlap is "true", for the built-in output checks whose result can
// be derived statically.
func siblingOverlapOutcome(gate apiv1.Gate) (string, bool) {
	if gate.Automated == nil || gate.Automated.Params["key"] != siblingOverlapOutputKey {
		return "", false
	}
	params := gate.Automated.Params
	var pass bool
	switch gate.Automated.Check {
	case "output-equals":
		pass = params["equals"] == "true"
	case "output-not-equals":
		pass = params["equals"] != "true"
	case "output-matches":
		re, err := regexp.Compile(params["pattern"])
		if err != nil {
			return "", false
		}
		pass = re.MatchString("true")
	default:
		return "", false
	}
	if pass {
		return string(apiv1.VerdictPass), true
	}
	return string(apiv1.VerdictFail), true
}

func describeTarget(target string) string {
	if target == TerminalComplete {
		return `"" (complete)`
	}
	return fmt.Sprintf("%q", target)
}

type overlapGraph struct {
	tasks     map[string]apiv1.Task
	gates     map[string]apiv1.Gate
	parallels map[string]apiv1.Parallel
}

func newOverlapGraph(spec apiv1.WorkflowSpec) overlapGraph {
	g := overlapGraph{
		tasks:     map[string]apiv1.Task{},
		gates:     map[string]apiv1.Gate{},
		parallels: map[string]apiv1.Parallel{},
	}
	for _, task := range spec.Tasks {
		g.tasks[task.Name] = task
	}
	for _, gate := range spec.Gates {
		g.gates[gate.Name] = gate
	}
	for _, parallel := range spec.Parallels {
		g.parallels[parallel.Name] = parallel
	}
	return g
}

type overlapVisit struct {
	state    string
	inBranch bool
}

// alwaysSequences reports whether every normally reachable path from target
// reaches a sequencing stage or an operator-visible terminal.
//
// It is the greatest fixpoint of the per-state rule: every state starts
// "sequences" and is demoted once some normal successor is silent, until
// nothing changes. A loop therefore only fails through a silent exit, and the
// result does not depend on visit order. inBranch marks evaluation inside a
// parallel arm, where "@join" hands control to the join instead of proving
// anything.
func (g overlapGraph) alwaysSequences(target string) bool {
	safe := map[overlapVisit]bool{}
	for _, inBranch := range []bool{false, true} {
		for name := range g.tasks {
			safe[overlapVisit{name, inBranch}] = true
		}
		for name := range g.gates {
			safe[overlapVisit{name, inBranch}] = true
		}
		for name := range g.parallels {
			safe[overlapVisit{name, inBranch}] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for key, ok := range safe {
			if ok && !g.stateSequences(key, safe) {
				safe[key] = false
				changed = true
			}
		}
	}
	return g.targetSequences(target, false, safe)
}

func (g overlapGraph) targetSequences(target string, inBranch bool, safe map[overlapVisit]bool) bool {
	switch target {
	case TerminalComplete, TargetAbort:
		return false
	case TargetEscalate:
		return true
	case TargetJoin:
		return !inBranch
	}
	result, known := safe[overlapVisit{target, inBranch}]
	// A dangling reference is reported elsewhere; do not guess here.
	return result || !known
}

func (g overlapGraph) stateSequences(key overlapVisit, safe map[overlapVisit]bool) bool {
	next := func(target string) bool { return g.targetSequences(target, key.inBranch, safe) }
	if task, ok := g.tasks[key.state]; ok {
		return isSequencingTask(task) || next(task.Next)
	}
	if gate, ok := g.gates[key.state]; ok {
		for outcome, target := range gate.Branches {
			// A runner-forced escalation is not a route the overlap takes.
			if outcome != BranchEscalate && !next(target) {
				return false
			}
		}
		return true
	}
	parallel := g.parallels[key.state]
	// fail_fast can abandon a sequencing arm and skip the join, so its
	// failure route must sequence on its own.
	if parallel.FailurePolicy == apiv1.BranchFailFast && parallel.OnFailure != "" && !next(parallel.OnFailure) {
		return false
	}
	// An arm that can end the run silently (rather than reach "@join")
	// aborts its unsettled siblings, so no other arm can cover it.
	covered := false
	for _, branch := range parallel.Branches {
		if branch.Start == "" {
			continue
		}
		if !g.targetSequences(branch.Start, false, safe) {
			return false
		}
		// Otherwise every arm settles, so one that always sequences
		// covers the run.
		covered = covered || g.targetSequences(branch.Start, true, safe)
	}
	if covered {
		return true
	}
	for _, target := range []string{parallel.Join, parallel.OnFailure} {
		if target != "" && !next(target) {
			return false
		}
	}
	return true
}

func isSequencingTask(task apiv1.Task) bool {
	for _, action := range task.PolicyActions {
		if slices.Contains(sequencingPolicyActions, strings.TrimSpace(action)) {
			return true
		}
	}
	if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
		return false
	}
	return slices.Contains(sequencingBuiltins, task.Run.Command[1])
}
