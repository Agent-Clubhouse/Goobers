package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func resolveTaskEnvelopeInputs(inputs map[string]interface{}, tf taskFrame) error {
	t, in := tf.t, tf.in
	for inputKey, outputKey := range t.InputsFrom {
		if v, ok, branchRef, absent := resolveBranchInput(outputKey, in.Machine, tf.completed, tf.fanIn); branchRef {
			if ok {
				inputs[inputKey] = v
			} else if absent {
				delete(inputs, inputKey)
			} else {
				return branchInputsFromError(t.Name, inputKey, outputKey)
			}
			continue
		}
		qualified := workflow.SupportsStageQualifiedInputs(in.Machine)
		v, ok := resolveInputsFrom(outputKey, tf.upstreamResult, tf.completed, qualified)
		if !ok {
			return inputsFromError(t.Name, inputKey, outputKey, tf.completed, qualified)
		}
		inputs[inputKey] = v
	}
	if tf.fanIn != nil && t.Name == tf.fanIn.spec.Join {
		inputs[BranchCompletenessInput] = tf.fanIn.completeness()
	}
	return nil
}

func (r *Runner) automatedGateEnvelope(in StartInput, g apiv1.Gate, limits apiv1.Limits, upstream []apiv1.ContextPointer) apiv1.InvocationEnvelope {
	baseBranch := in.RepoRef.Branch
	if baseBranch == "" {
		baseBranch = "main"
	}
	return apiv1.InvocationEnvelope{
		TaskID:                 in.RunID + ":" + g.Name,
		InstanceID:             in.instanceID,
		WorkflowID:             in.Machine.Def.Name,
		RunID:                  in.RunID,
		TriggerRef:             in.Trigger.Ref,
		Gaggle:                 in.Gaggle,
		BranchNamespace:        r.branchNamespaceFor(in.Gaggle),
		BaseBranch:             baseBranch,
		Goal:                   "gate: " + g.Name,
		RepoRef:                in.RepoRef.EnvelopeRef(),
		WorkspaceRevision:      in.workspaceRevision.DeepCopy(),
		WorkspaceBranchBinding: in.workspaceBranchBinding.DeepCopy(),
		Item:                   in.Item,
		Limits:                 limits,
		ContextPointers:        append([]apiv1.ContextPointer(nil), upstream...),
	}
}
