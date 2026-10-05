package workflow

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow/internal/model"
)

// compileContainedParallel overlays only the frozen interpreter's shared-branch
// workspace restriction. The original definition is fully compiled first; any
// other finding remains fatal. The projection supplies graph topology only.
// Runtime lookups and the digest always contain the exact original definition.
func compileContainedParallel(def Definition, config compileConfig, original error) (*Machine, error) {
	if original == nil || len(def.Spec.Parallels) == 0 || len(def.Spec.Gates) != 0 {
		return nil, original
	}
	tasks := make(map[string]apiv1.Task, len(def.Spec.Tasks))
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskAgentic || task.ChildWorkflows == nil || task.EffectiveWorkspace() != apiv1.WorkspaceRepo {
			return nil, original
		}
		tasks[task.Name] = task
	}
	remaining := strings.TrimPrefix(original.Error(), fmt.Sprintf("invalid workflow %q: ", def.Name))
	for _, parallel := range def.Spec.Parallels {
		for _, branch := range parallel.Branches {
			seen := map[string]bool{}
			for state := branch.Start; state != "" && !seen[state]; {
				seen[state] = true
				task, ok := tasks[state]
				if !ok {
					break
				}
				message := fmt.Sprintf("parallel %q branch %q: task %q resolves to a writable repo workspace; branch stages must use scratch or repo-readonly (concurrent repo-backed branches collide on the run branch)", parallel.Name, branch.Name, state)
				remaining = strings.ReplaceAll(remaining, message, "")
				state = task.Next
			}
		}
	}
	if strings.Trim(remaining, "; ") != "" {
		return nil, original
	}
	projection := def
	projection.Spec.Tasks = append([]apiv1.Task(nil), def.Spec.Tasks...)
	for i := range projection.Spec.Tasks {
		projection.Spec.Tasks[i].Workspace = apiv1.WorkspaceRepoReadOnly
		projection.Spec.Tasks[i].RepoFrom = nil
	}
	graph, err := compileV30Base(projection, config)
	if err != nil {
		return nil, err
	}
	parallels := make(map[string]apiv1.Parallel, len(def.Spec.Parallels))
	for _, parallel := range def.Spec.Parallels {
		parallels[parallel.Name] = parallel
	}
	return model.NewMachine(def, tasks, map[string]apiv1.Gate{}, parallels, graph.Graph())
}
