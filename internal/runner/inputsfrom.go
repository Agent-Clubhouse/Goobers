package runner

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// stageOutputs is every completed stage's journaled Outputs, keyed by stage
// name. The runner already records these; #562 just makes them addressable.
type stageOutputs map[string]map[string]any

func (s stageOutputs) record(stage string, outputs map[string]any) {
	if stage == "" || len(outputs) == 0 {
		return
	}
	if s == nil {
		return
	}
	copied := make(map[string]any, len(outputs))
	for k, v := range outputs {
		copied[k] = v
	}
	s[stage] = copied
}

// resolveInputsFrom resolves one inputsFrom value against the run's completed
// stage outputs.
//
// The resolution rule is deliberately conservative, and it is the rule a prior
// attempt at #562 parked on getting wrong:
//
//	Treat the value as a STAGE-QUALIFIED reference only when the segment
//	before the first dot names a stage that has actually produced outputs in
//	this run. Otherwise treat the ENTIRE string as a bare output key.
//
// That ordering is what preserves legacy dotted keys — an output literally
// named "a.b" keeps working, because "a" is not a stage. Stage names containing
// a dot are rejected at compile time, so there is no ambiguity to escape and no
// escaping syntax to invent.
//
// Bare keys keep their exact current meaning: they resolve against the
// immediately preceding stage only. That is what makes this fully
// backward-compatible.
func resolveInputsFrom(value string, upstream apiv1.ResultEnvelope, completed stageOutputs, qualified bool) (any, bool) {
	if stage, key, ok := splitQualified(value); ok && qualified {
		if outputs, seen := completed[stage]; seen {
			v, found := outputs[key]
			return v, found
		}
		// The prefix is not a stage that ran — fall through and treat the whole
		// value as a bare key, which is what keeps a legacy dotted key working.
	}
	v, ok := upstream.Outputs[value]
	return v, ok
}

// splitQualified splits "<stage>.<key>" on the FIRST dot. An output key may
// itself contain dots; a stage name may not.
func splitQualified(value string) (stage, key string, ok bool) {
	stage, key, found := strings.Cut(value, ".")
	if !found || stage == "" || key == "" {
		return "", "", false
	}
	return stage, key, true
}

type branchInputRef struct {
	parallel string
	branch   string
	stage    string
	key      string
}

func splitBranchInput(value string) (branchInputRef, bool) {
	parts := strings.SplitN(value, ".", 4)
	switch len(parts) {
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return branchInputRef{}, false
		}
		return branchInputRef{parallel: parts[0], branch: parts[1], key: parts[2]}, true
	case 4:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
			return branchInputRef{}, false
		}
		return branchInputRef{parallel: parts[0], branch: parts[1], stage: parts[2], key: parts[3]}, true
	default:
		return branchInputRef{}, false
	}
}

func resolveBranchInput(value string, machine *workflow.Machine, completed stageOutputs, fanIn *parallelExec) (any, bool, bool, bool) {
	ref, ok := splitBranchInput(value)
	if !ok || fanIn == nil || ref.parallel != fanIn.spec.Name {
		return nil, false, false, false
	}
	branch := fanIn.branch(ref.branch)
	if branch == nil {
		return nil, false, true, false
	}
	switch branch.status {
	case journal.BranchFailed, journal.BranchTimedOut, journal.BranchCancelled:
		return nil, false, true, true
	case journal.BranchNoOutput:
		return nil, false, true, true
	}
	if ref.stage == "" {
		ref.stage = singleJoinTerminalTask(machine, branch.start)
	}
	outputs, seen := completed[ref.stage]
	if !seen {
		return nil, false, true, false
	}
	valueOut, found := outputs[ref.key]
	return valueOut, found, true, false
}

func singleJoinTerminalTask(machine *workflow.Machine, start string) string {
	seen := map[string]bool{}
	stack := []string{start}
	var terminal string
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == workflow.TerminalComplete || workflow.IsReservedAnyTarget(state) || seen[state] || !machine.Has(state) {
			continue
		}
		seen[state] = true
		if task, ok := machine.Task(state); ok && task.Next == workflow.TargetJoin {
			if terminal != "" {
				return ""
			}
			terminal = state
		}
		stack = append(stack, machine.Outgoing(state)...)
	}
	return terminal
}

func branchInputsFromError(taskName, inputKey, value string) error {
	return fmt.Errorf("task %q: inputsFrom %q: branch output %q not found", taskName, inputKey, value)
}

// inputsFromError builds the stage-closed failure for an unresolvable
// reference. InputsFrom is a contract, not a hint, so a miss fails the stage
// rather than silently omitting the input.
func inputsFromError(taskName, inputKey, value string, completed stageOutputs, qualified bool) error {
	if stage, key, ok := splitQualified(value); ok && qualified {
		if outputs, seen := completed[stage]; seen {
			return fmt.Errorf("task %q: inputsFrom %q: stage %q produced no output %q (it emitted: %s)",
				taskName, inputKey, stage, key, joinKeys(outputs))
		}
	}
	return fmt.Errorf("task %q: inputsFrom %q: upstream output %q not found", taskName, inputKey, value)
}

func joinKeys(outputs map[string]any) string {
	if len(outputs) == 0 {
		return "nothing"
	}
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return strings.Join(keys, ", ")
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// reconstructStageOutputs rebuilds every completed stage's Outputs from the
// journal, so a resumed run can resolve a stage-qualified inputsFrom reference
// to a stage that finished before the crash.
//
// Without this, a qualified reference would resolve on a live run and fail on a
// resumed one — the resume-divergence class that is silent outside a crash. It
// is deliberately a FORWARD scan with last-write-wins per stage, so a repassed
// stage's later attempt supersedes its earlier one, matching what the live walk
// does when it re-records the stage.
func reconstructStageOutputs(events []journal.Event) stageOutputs {
	out := stageOutputs{}
	for _, e := range events {
		if e.Type != journal.EventStageFinished || e.Stage == "" || len(e.Outputs) == 0 {
			continue
		}
		out.record(e.Stage, e.Outputs)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
