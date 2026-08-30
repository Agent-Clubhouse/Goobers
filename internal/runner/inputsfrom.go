package runner

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// stageOutput is one completed stage's journaled Outputs together with the
// provenance of the content that produced them. Outputs are bare scalars with
// nowhere to carry a label of their own, so a consumer resolving inputsFrom
// grades the producing stage (TBH-4).
type stageOutput struct {
	outputs   map[string]any
	integrity apiv1.Integrity
}

// stageOutputs is every completed stage's journaled Outputs, keyed by stage
// name. The runner already records these; #562 just makes them addressable.
type stageOutputs map[string]stageOutput

func (s stageOutputs) record(stage string, outputs map[string]any, grade apiv1.Integrity) {
	if stage == "" {
		return
	}
	if s == nil {
		return
	}
	copied := make(map[string]any, len(outputs))
	for k, v := range outputs {
		copied[k] = v
	}
	s[stage] = stageOutput{outputs: copied, integrity: grade}
}

// put copies an already-recorded stage entry, preserving its provenance. Used
// when merging one stageOutputs into another (branch fan-in, clones).
func (s stageOutputs) put(stage string, produced stageOutput) {
	if stage == "" || s == nil {
		return
	}
	s.record(stage, produced.outputs, produced.integrity)
}

// integrityOf returns the provenance recorded for a completed stage. An unknown
// stage returns the zero grade, which fails admission closed.
func (s stageOutputs) integrityOf(stage string) apiv1.Integrity {
	return s[stage].integrity
}

func (s stageOutputs) clear(stage string) {
	if s != nil {
		delete(s, stage)
	}
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
		if produced, seen := completed[stage]; seen {
			v, found := produced.outputs[key]
			return v, found
		}
		// The prefix is not a stage that ran — fall through and treat the whole
		// value as a bare key, which is what keeps a legacy dotted key working.
	}
	v, ok := upstream.Outputs[value]
	return v, ok
}

// inputsFromIntegrity returns the provenance of whatever resolveInputsFrom would
// bind for value. It deliberately mirrors that function's branch order rather
// than re-deriving it: the grade must describe the value actually bound, so if
// the two ever disagree the admission check would be guarding the wrong stage.
func inputsFromIntegrity(value string, upstream apiv1.ResultEnvelope, completed stageOutputs, qualified bool) apiv1.Integrity {
	if stage, _, ok := splitQualified(value); ok && qualified {
		if _, seen := completed[stage]; seen {
			return completed.integrityOf(stage)
		}
	}
	return upstream.Integrity
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
	produced, seen := completed[ref.stage]
	if !seen {
		return nil, false, true, false
	}
	valueOut, found := produced.outputs[ref.key]
	return valueOut, found, true, false
}

// branchInputIntegrity returns the provenance of the branch output
// resolveBranchInput would bind, or the zero grade when the reference does not
// resolve to a recorded stage (which fails admission closed).
func branchInputIntegrity(value string, machine *workflow.Machine, completed stageOutputs, fanIn *parallelExec) apiv1.Integrity {
	ref, ok := splitBranchInput(value)
	if !ok || fanIn == nil || ref.parallel != fanIn.spec.Name {
		return ""
	}
	branch := fanIn.branch(ref.branch)
	if branch == nil {
		return ""
	}
	if ref.stage == "" {
		ref.stage = singleJoinTerminalTask(machine, branch.start)
	}
	return completed.integrityOf(ref.stage)
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
		if produced, seen := completed[stage]; seen {
			return fmt.Errorf("task %q: inputsFrom %q: stage %q produced no output %q (it emitted: %s)",
				taskName, inputKey, stage, key, joinKeys(produced.outputs))
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

func discardToleratedFailureOutputs(machine *workflow.Machine, stage string, result apiv1.ResultEnvelope) apiv1.ResultEnvelope {
	if machine == nil || result.Status != apiv1.ResultFailure {
		return result
	}
	if task, ok := machine.Task(stage); ok && task.ContinueOnError {
		result.Outputs = nil
	}
	return result
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
func reconstructStageOutputs(events []journal.Event, machine *workflow.Machine) stageOutputs {
	out := stageOutputs{}
	for _, e := range events {
		if e.Type != journal.EventStageFinished || e.Stage == "" {
			continue
		}
		if machine != nil {
			if task, ok := machine.Task(e.Stage); ok &&
				e.Status == string(apiv1.ResultFailure) && task.ContinueOnError {
				out.clear(e.Stage)
				continue
			}
		}
		out.record(e.Stage, e.Outputs, e.Integrity)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolvedInputGrades maps each of a task's inputsFrom entries to the provenance
// of the stage that will produce it, keyed by the consuming input name so an
// admission failure names the input the workflow author declared.
//
// It mirrors dispatchTask's resolution order exactly — branch reference first,
// then stage-qualified/bare — because a grade that described a different stage
// than the one actually bound would be worse than no check at all.
func resolvedInputGrades(
	t apiv1.Task,
	machine *workflow.Machine,
	upstreamResult apiv1.ResultEnvelope,
	completed stageOutputs,
	fanIn *parallelExec,
) map[string]apiv1.Integrity {
	if len(t.InputsFrom) == 0 {
		return nil
	}
	grades := make(map[string]apiv1.Integrity, len(t.InputsFrom))
	for inputKey, outputKey := range t.InputsFrom {
		if _, _, branchRef, absent := resolveBranchInput(outputKey, machine, completed, fanIn); branchRef {
			if absent {
				// dispatchTask drops an absent branch input rather than binding
				// it, so there is no content to grade.
				continue
			}
			grades[inputKey] = branchInputIntegrity(outputKey, machine, completed, fanIn)
			continue
		}
		qualified := workflow.SupportsStageQualifiedInputs(machine)
		grades[inputKey] = inputsFromIntegrity(outputKey, upstreamResult, completed, qualified)
	}
	return grades
}

// producedIntegrity grades what a stage emitted. Provenance flows with the data:
// output is only as trustworthy as the weakest input the stage was admitted
// with, so an agent that reads unapproved text cannot launder it into a
// maintainer-graded output.
//
// An agentic stage always contributes IntegrityDerived, which keeps agent output
// distinguishable from maintainer-authored input while still satisfying a
// maintainer minimum (see Grade.Meets). A deterministic stage with no graded
// input ran purely from operator config, so it produces trusted content.
func producedIntegrity(
	t apiv1.Task,
	item *apiv1.BacklogItem,
	pointers []apiv1.ContextPointer,
	inputGrades map[string]apiv1.Integrity,
) apiv1.Integrity {
	grades := make([]apiv1.Integrity, 0, len(pointers)+len(inputGrades)+2)
	if t.Type == apiv1.TaskAgentic {
		grades = append(grades, apiv1.IntegrityDerived)
	}
	if item != nil && item.Integrity != "" {
		grades = append(grades, item.Integrity)
	}
	for i := range pointers {
		grade := pointers[i].Integrity
		if grade == "" && pointers[i].Artifact != nil {
			grade = pointers[i].Artifact.Integrity
		}
		if grade != "" {
			grades = append(grades, grade)
		}
	}
	for _, grade := range inputGrades {
		if grade != "" {
			grades = append(grades, grade)
		}
	}
	if len(grades) == 0 {
		return apiv1.IntegrityTrusted
	}
	return apiv1.WeakestIntegrity(grades...)
}

// --- shared with the Temporal engine (#624 shared-constant pattern) ---------
//
// Plan item E2 (#3874) ports stage-qualified inputsFrom resolution to the
// Temporal engine's walk. The RESOLUTION ORDER — qualified only when the
// prefix names a stage that ran, bare key otherwise — is not re-implemented
// there. It is shared from here, for the same reason the retry classification
// is (RetryFailureClass): it is the rule that decides what an already-released
// DSL version MEANS, and two copies of it would eventually bind different
// values on the two runners for the same definition. A drifted copy would not
// fail loudly; it would silently hand a stage the wrong input.
//
// The exported surface is deliberately the minimum the engine's walk needs:
// build the map, record/forget a stage, resolve a value, grade it, and phrase
// the miss. Everything else here — branch fan-in, the parallel-branch refs —
// stays runner-private because the engine has no counterpart for it.

// StageOutputs is every completed stage's Outputs keyed by stage name, the
// state stage-qualified resolution reads. It is an alias rather than a defined
// type so the runner's own internals keep using the unexported spelling.
type StageOutputs = stageOutputs

// NewStageOutputs builds an empty completed-stage map.
func NewStageOutputs() StageOutputs { return stageOutputs{} }

// Record copies a finished stage's outputs under its name, with the provenance
// grade of the content that produced them. The copy matters: a walk keeps
// handing the same ResultEnvelope onward, and a shared map would let a later
// mutation rewrite an earlier stage's recorded outputs.
func (s stageOutputs) Record(stage string, outputs map[string]any, grade apiv1.Integrity) {
	s.record(stage, outputs, grade)
}

// Forget discards a stage's outputs. This is what a TOLERATED failure must do:
// downstream stages must not consume partial results, and a qualified reference
// to the forgotten stage falls through to bare-key resolution.
func (s stageOutputs) Forget(stage string) { s.clear(stage) }

// IntegrityOf returns the provenance recorded for a completed stage. An unknown
// stage returns the zero grade, which fails admission closed.
func (s stageOutputs) IntegrityOf(stage string) apiv1.Integrity { return s.integrityOf(stage) }

// RecordCompleted applies the record-or-forget rule a finished task owes the
// map: a tolerated failure (ContinueOnError) is FORGOTTEN, anything else is
// recorded. Shared because "which results become addressable" is the same
// question as "what does <stage>.<key> resolve to", and answering it
// differently on the two runners is the divergence this whole export exists to
// prevent.
func (s stageOutputs) RecordCompleted(t apiv1.Task, result apiv1.ResultEnvelope) {
	if result.Status == apiv1.ResultFailure && t.ContinueOnError {
		s.clear(t.Name)
		return
	}
	s.record(t.Name, result.Outputs, result.Integrity)
}

// ResolveInputsFrom exports resolveInputsFrom for the engine walk.
func ResolveInputsFrom(value string, upstream apiv1.ResultEnvelope, completed StageOutputs, qualified bool) (any, bool) {
	return resolveInputsFrom(value, upstream, completed, qualified)
}

// InputsFromIntegrity exports inputsFromIntegrity — the provenance of whatever
// ResolveInputsFrom would bind for the same arguments.
func InputsFromIntegrity(value string, upstream apiv1.ResultEnvelope, completed StageOutputs, qualified bool) apiv1.Integrity {
	return inputsFromIntegrity(value, upstream, completed, qualified)
}

// InputsFromError exports inputsFromError, so an unresolvable reference reads
// identically to an operator whichever runner walked the definition.
func InputsFromError(taskName, inputKey, value string, completed StageOutputs, qualified bool) error {
	return inputsFromError(taskName, inputKey, value, completed, qualified)
}
