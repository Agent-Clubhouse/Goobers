package workflow

import (
	"fmt"
	"maps"
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
// The analysis is structural and conservative: an unresolvable target, an
// "@escalate" (operator-visible) or "@join" target, or any reachable
// sequencing stage on some path suppresses the finding. Only a branch whose
// every path ends at completion or "@abort" is reported.
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
		if g.reachesSequencing(target, map[string]bool{}) {
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

// reachesSequencing reports whether some path from state reaches a
// sequencing stage, or a target this analysis cannot prove silent.
func (g overlapGraph) reachesSequencing(state string, visited map[string]bool) bool {
	switch state {
	case TerminalComplete, TargetAbort:
		return false
	case TargetEscalate, TargetJoin:
		return true
	}
	if visited[state] {
		return false
	}
	visited[state] = true
	if task, ok := g.tasks[state]; ok {
		return isSequencingTask(task) || g.reachesSequencing(task.Next, visited)
	}
	if gate, ok := g.gates[state]; ok {
		for _, outcome := range slices.Sorted(maps.Keys(gate.Branches)) {
			// A runner-forced escalation is not a route the overlap takes.
			if outcome == BranchEscalate {
				continue
			}
			if g.reachesSequencing(gate.Branches[outcome], visited) {
				return true
			}
		}
		return false
	}
	if parallel, ok := g.parallels[state]; ok {
		targets := []string{parallel.Join, parallel.OnFailure}
		for _, branch := range parallel.Branches {
			targets = append(targets, branch.Start)
		}
		for _, target := range targets {
			if target != "" && g.reachesSequencing(target, visited) {
				return true
			}
		}
		return false
	}
	// A dangling reference is reported elsewhere; do not guess here.
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
