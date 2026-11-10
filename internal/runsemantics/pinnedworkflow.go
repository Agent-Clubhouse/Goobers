package runsemantics

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// PinnedWorkflowMachine reconstructs the historical machine from the trusted,
// content-addressed Definition snapshot journaled at Start and verifies every
// identity boundary: the input's trusted integrity, the definition's
// name/version, and — twice, once over the raw snapshot and once over the
// compiled machine — the run's pinned WorkflowDigest (WF-016). A caller that
// resumes or intervenes on a run whose pin no longer matches the current
// config should prefer this machine over the current one (#3376): the run
// keeps walking the definition it started under, while any snapshot that is
// missing, untrusted, corrupt, or mismatched still fails closed here and the
// WF-016 refusal stands.
func PinnedWorkflowMachine(rd *journal.Reader, id journal.RunIdentity) (*workflow.Machine, error) {
	return PinnedWorkflowMachineFrom(rd, id)
}

// PinnedWorkflowMachineFrom is PinnedWorkflowMachine over any journal backend's
// run reader (the cloud's PostgreSQL journal among them).
func PinnedWorkflowMachineFrom(rd journal.RunReader, id journal.RunIdentity) (*workflow.Machine, error) {
	var definitionRef *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name == journal.PinnedWorkflowDefinitionInputName {
			definitionRef = &id.Inputs[i]
			break
		}
	}
	if definitionRef == nil {
		return nil, fmt.Errorf("immutable input %q is missing", journal.PinnedWorkflowDefinitionInputName)
	}
	if definitionRef.Integrity != apiv1.IntegrityTrusted {
		return nil, fmt.Errorf("immutable input %q has integrity %q, want %q",
			journal.PinnedWorkflowDefinitionInputName, definitionRef.Integrity, apiv1.IntegrityTrusted)
	}
	data, err := rd.ArtifactBytes(definitionRef.Ref)
	if err != nil {
		return nil, fmt.Errorf("read immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	var def workflow.Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parse immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	if def.Name != id.Workflow || def.Version != id.WorkflowVersion {
		return nil, fmt.Errorf(
			"immutable input %q identifies workflow %q version %d, want %q version %d",
			journal.PinnedWorkflowDefinitionInputName, def.Name, def.Version, id.Workflow, id.WorkflowVersion,
		)
	}
	digest, err := workflow.ComputeDigest(def)
	if err != nil {
		return nil, fmt.Errorf("digest immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	if digest != id.WorkflowDigest {
		return nil, fmt.Errorf(
			"immutable input %q digest %q does not match run pin %q",
			journal.PinnedWorkflowDefinitionInputName, digest, id.WorkflowDigest,
		)
	}
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		return nil, fmt.Errorf("compile immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	if machine.Digest() != id.WorkflowDigest {
		return nil, fmt.Errorf(
			"compiled immutable input %q digest %q does not match run pin %q",
			journal.PinnedWorkflowDefinitionInputName, machine.Digest(), id.WorkflowDigest,
		)
	}
	return machine, nil
}
