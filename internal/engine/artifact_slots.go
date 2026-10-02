package engine

import (
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const namedPublicationChange = "named-artifact-publication"

// A replay of an older history must retain its original invocation payload and
// journal projection, even when its admitted 3.1 task declared advisory slots.
func namedPublicationTask(ctx workflow.Context, task apiv1.Task) apiv1.Task {
	if len(task.ArtifactSlots) > 0 && workflow.GetVersion(ctx, namedPublicationChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		task.ArtifactSlots = nil
	}
	return task
}
