package runner

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

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

func resolveBranchInput(value string, machine *workflow.Machine, completed runsemantics.StageOutputs, fanIn *parallelExec) (any, bool, bool, bool) {
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
	case journal.BranchNoOutput, journal.BranchSkipped:
		return nil, false, true, true
	}
	if ref.stage == "" {
		ref.stage = singleJoinTerminalTask(machine, branch.start)
	}
	produced, seen := completed[ref.stage]
	if !seen {
		return nil, false, true, false
	}
	valueOut, found := produced.Outputs[ref.key]
	return valueOut, found, true, false
}

// branchInputIntegrity returns the provenance of the branch output
// resolveBranchInput would bind, or the zero grade when the reference does not
// resolve to a recorded stage (which fails admission closed).
func branchInputIntegrity(value string, machine *workflow.Machine, completed runsemantics.StageOutputs, fanIn *parallelExec) apiv1.Integrity {
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
	return completed.IntegrityOf(ref.stage)
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
func reconstructStageOutputs(events []journal.Event, machine *workflow.Machine) runsemantics.StageOutputs {
	out := runsemantics.StageOutputs{}
	for _, e := range events {
		if e.Type != journal.EventStageFinished || e.Stage == "" {
			continue
		}
		if isMappedTask(machine, e.Stage) {
			// Mapped results are per item; only the per-occurrence store
			// holds them (design plan-driven-dynamic-fan-out.md §6.3).
			continue
		}
		if machine != nil {
			if task, ok := machine.Task(e.Stage); ok &&
				e.Status == string(apiv1.ResultFailure) && task.ContinueOnError {
				out.Clear(e.Stage)
				continue
			}
		}
		out.Record(e.Stage, e.Outputs, e.Integrity)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func reconstructWorkspaceRevision(events []journal.Event, machine *workflow.Machine) (*apiv1.WorkspaceRevision, error) {
	var revision *apiv1.WorkspaceRevision
	for _, event := range events {
		if event.WorkspaceRevision == nil {
			continue
		}
		if event.Type != journal.EventStageFinished || event.Status != string(apiv1.ResultSuccess) {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "journal workspace revision requires a successful stage.finished event"}
		}
		if machine == nil {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "journal workspace revision requires a pinned workflow"}
		}
		task, ok := machine.Task(event.Stage)
		if !ok {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "journal workspace revision producer is not a pinned task"}
		}
		result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: event.WorkspaceRevision}
		if err := workspacerevision.NormalizeResult(&result, task.Type == apiv1.TaskDeterministic); err != nil {
			return nil, err
		}
		var err error
		revision, err = workspacerevision.Accept(revision, result.WorkspaceRevision)
		if err != nil {
			return nil, err
		}
	}
	return revision, nil
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
	completed runsemantics.StageOutputs,
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
		grades[inputKey] = runsemantics.InputsFromIntegrity(outputKey, upstreamResult, completed, qualified)
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
// build the map, apply the record-or-forget rule to a finished task, resolve a
// value, grade it, and phrase the miss. Note that record and forget are NOT
// exported individually — only the rule that chooses between them is, so the
// two runners cannot disagree about which results become addressable.
// Everything else here — branch fan-in, the parallel-branch refs — stays
// runner-private because the engine has no counterpart for it.
