package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func resolveBranchInput(value string, machine *workflow.Machine, completed runsemantics.StageOutputs, fanIn *parallelExec) (any, bool, bool, bool) {
	resolved := resolveRunnerBranchInput(value, machine, completed, fanIn)
	return resolved.Value, resolved.Found, resolved.Recognized, resolved.Absent
}

// branchInputIntegrity uses the same resolution as the bound value.
func branchInputIntegrity(value string, machine *workflow.Machine, completed runsemantics.StageOutputs, fanIn *parallelExec) apiv1.Integrity {
	return resolveRunnerBranchInput(value, machine, completed, fanIn).Integrity
}

func resolveRunnerBranchInput(value string, machine *workflow.Machine, completed runsemantics.StageOutputs, fanIn *parallelExec) runsemantics.BranchInputResolution {
	if fanIn == nil {
		return runsemantics.BranchInputResolution{}
	}
	fanIn.mu.Lock()
	defer fanIn.mu.Unlock()
	branches := make(map[string]runsemantics.BranchInputSource, len(fanIn.branches))
	for _, branch := range fanIn.branches {
		branches[branch.name] = runsemantics.BranchInputSource{Start: branch.start, Status: branch.status, Completed: completed}
	}
	return runsemantics.ResolveBranchInput(value, machine, fanIn.spec.Name, branches)
}

func branchInputsFromError(taskName, inputKey, value string) error {
	return runsemantics.BranchInputsFromError(taskName, inputKey, value)
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
