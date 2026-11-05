package engine

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runsemantics"
	wf "github.com/goobers/goobers/internal/workflow"
)

const staticBranchInputsVersion = "engine-static-branch-inputs-v1"

// joinSources uses this visit's settled branch products, never the root's seed
// or a previous visit's results. Values and grades travel through one resolver.
func (p *staticParallelWalk) joinSources() map[string]runsemantics.BranchInputSource {
	sources := make(map[string]runsemantics.BranchInputSource, len(p.spec.Branches))
	for i, branch := range p.spec.Branches {
		completed := runsemantics.NewStageOutputs()
		for name, result := range p.outcomes[i].frame.results {
			if task, ok := p.machine.Task(name); ok {
				completed.RecordCompleted(task, result)
			}
		}
		sources[branch.Name] = runsemantics.BranchInputSource{Start: branch.Start, Status: p.outcomes[i].frame.status, Completed: completed}
	}
	return sources
}

func (r *runJournal) branchInput(stage, value string, m *wf.Machine) runsemantics.BranchInputResolution {
	if s := r.staticWalk; s != nil && s.join == stage {
		return runsemantics.ResolveBranchInput(value, m, s.parallel, s.branches)
	}
	return runsemantics.BranchInputResolution{}
}

func (r *runJournal) taskInputGrades(t apiv1.Task, m *wf.Machine, upstream apiv1.ResultEnvelope, completed completedStages, qualified bool) map[string]apiv1.Integrity {
	grades := engineInputGrades(t, upstream, completed, qualified)
	for key, ref := range t.InputsFrom {
		resolved := r.branchInput(t.Name, ref, m)
		if !resolved.Recognized {
			continue
		}
		if resolved.Absent {
			delete(grades, key)
		} else {
			grades[key] = resolved.Integrity
		}
	}
	return grades
}

func (r *runJournal) bindTaskInputs(t apiv1.Task, m *wf.Machine, upstream apiv1.ResultEnvelope, completed completedStages, qualified bool, inputs map[string]any) error {
	for _, key := range sortedKeys(t.InputsFrom) {
		ref := t.InputsFrom[key]
		resolved := r.branchInput(t.Name, ref, m)
		if resolved.Recognized {
			switch {
			case resolved.Found:
				inputs[key] = resolved.Value
			case resolved.Absent:
				delete(inputs, key)
			default:
				return runsemantics.BranchInputsFromError(t.Name, key, ref)
			}
			continue
		}
		value, ok := resolveInputsFrom(ref, upstream, completed, qualified)
		if !ok {
			return inputsFromError(t.Name, key, ref, upstream, completed, qualified)
		}
		inputs[key] = value
	}
	return nil
}
