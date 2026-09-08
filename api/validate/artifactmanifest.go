package validate

import (
	"fmt"
	"maps"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// checkArtifactManifestInputs validates the new manifest declaration without
// changing legacy artifactFile-only workflows. Runtime repeats these checks on
// resolved inputs, including values supplied through an inputsFrom handoff.
func checkArtifactManifestInputs(r *Report, w apiv1.Workflow, file string) {
	for i, task := range w.Spec.Tasks {
		checkArtifactManifestVariant(r, w, file, i, task, task.Inputs, "")
		if task.Experiment == nil {
			continue
		}
		for _, arm := range task.Experiment.Arms {
			inputs := make(map[string]string, len(task.Inputs)+len(arm.Variant))
			maps.Copy(inputs, task.Inputs)
			maps.Copy(inputs, arm.Variant)
			checkArtifactManifestVariant(r, w, file, i, task, inputs, " experiment arm "+arm.Name)
		}
	}
}

func checkArtifactManifestVariant(r *Report, w apiv1.Workflow, file string, i int, task apiv1.Task, inputs map[string]string, variant string) {
	manifest, hasManifest := inputs["artifactManifestFile"]
	_, dynamicManifest := task.InputsFrom["artifactManifestFile"]
	if !hasManifest && !dynamicManifest {
		return
	}
	_, legacy := inputs["artifactFile"]
	_, dynamicLegacy := task.InputsFrom["artifactFile"]
	var reason string
	switch {
	case legacy || dynamicLegacy:
		reason = "artifactManifestFile and artifactFile are mutually exclusive"
	case task.Type != "agentic":
		reason = "artifactManifestFile requires an agentic task"
	case !dynamicManifest:
		pointer := apiv1.ArtifactPointer{Path: manifest, Digest: apiv1.Digest(nil)}
		if manifest == "" || pointer.Validate() != nil {
			reason = "artifactManifestFile must be a contained workspace-relative file path"
		}
	}
	if reason != "" {
		r.add(errorStageRequiredInput, Error, file, "Workflow", w.Name, "%s%s: %s", fmt.Sprintf("spec.tasks[%d].inputs", i), variant, reason)
	}
}
