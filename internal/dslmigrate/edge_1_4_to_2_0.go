package dslmigrate

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	taskKindInput  = "kind"
	ciPollTaskKind = "ci-poll"
)

// applyCurrentToNext implements the DVL-5 (#865, PR #1353) v_current→v_2_0
// edge's one concrete semantic delta: internal/workflow/v_2_0's ci-poll
// input builder injects a "10s" pollIntervalSeconds default onto a ci-poll
// task's invocation whenever its downstream automated gate leaves
// PollIntervalSeconds unset, where v_current's builder leaves the input
// absent entirely (see internal/workflow/{v_current,v_2_0}/inputs.go). That
// default lives in compiled-invocation code, not in the workflow schema, so
// migrating a workflow forward would otherwise silently change its runtime
// polling cadence. This transform makes the new default explicit in the YAML
// — pinning `automated.pollIntervalSeconds: 10` on every such gate — so the
// compiled behavior is identical immediately before and after the version
// bump, and the change is visible in the migration diff for review.
func applyCurrentToNext(_ []byte, root *yaml.Node) (bool, []string, error) {
	var notes []string
	changed := false
	spec, _ := mapValue(root, "spec")
	if spec != nil {
		gatesByName := map[string]*yaml.Node{}
		if gates, _ := mapValue(spec, "gates"); gates != nil {
			for _, gate := range gates.Content {
				if name, _ := mapValue(gate, "name"); name != nil {
					gatesByName[name.Value] = gate
				}
			}
		}
		if tasks, _ := mapValue(spec, "tasks"); tasks != nil {
			for _, task := range tasks.Content {
				note, ok := pinCIPollInterval(task, gatesByName)
				if ok {
					changed = true
					notes = append(notes, note)
				}
			}
		}
	}
	return changed, notes, nil
}

func pinCIPollInterval(task *yaml.Node, gatesByName map[string]*yaml.Node) (string, bool) {
	inputs, _ := mapValue(task, "inputs")
	if inputs == nil {
		return "", false
	}
	kind, _ := mapValue(inputs, taskKindInput)
	if kind == nil || kind.Value != ciPollTaskKind {
		return "", false
	}
	next, _ := mapValue(task, "next")
	if next == nil || next.Value == "" {
		return "", false
	}
	gate, ok := gatesByName[next.Value]
	if !ok {
		return "", false
	}
	evaluator, _ := mapValue(gate, "evaluator")
	if evaluator == nil || evaluator.Value != "automated" {
		return "", false
	}
	automated, _ := mapValue(gate, "automated")
	if automated == nil {
		return "", false
	}
	if poll, _ := mapValue(automated, "pollIntervalSeconds"); poll != nil && poll.Value != "" && poll.Value != "0" {
		return "", false
	}
	setScalar(automated, "pollIntervalSeconds", "10", "!!int")
	taskName, _ := mapValue(task, "name")
	name := ciPollTaskKind
	if taskName != nil {
		name = taskName.Value
	}
	return fmt.Sprintf(
		"gate %q: pinned automated.pollIntervalSeconds: 10 explicitly — DSL 2.0's ci-poll input builder "+
			"injects this default for task %q where DSL 1.4 left it unset", next.Value, name,
	), true
}
