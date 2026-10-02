package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func (tf *taskFrame) recordTaskStarted(attempt int, class journal.AttemptClass) error {
	started := taskStartedEvent(tf.t, attempt, class)
	if len(tf.t.ArtifactSlots) > 0 {
		tf.artifactVisit = tf.jr.Seq() + 1
		if started.Runner == nil {
			started.Runner = map[string]any{}
		}
		started.Runner["artifactVisit"] = tf.artifactVisit
	}
	return tf.jr.Append(started)
}

func (tf taskFrame) pinPublicationAuthority(env *apiv1.InvocationEnvelope, attempt int) {
	t := tf.t
	env.MinimumIntegrity = t.MinimumIntegrity
	if len(t.ArtifactSlots) > 0 {
		env.ArtifactPublication = &apiv1.ArtifactPublication{Stage: t.Name, Visit: tf.artifactVisit, Slots: append([]apiv1.ArtifactSlot(nil), t.ArtifactSlots...)}
	}
	env.Attempt = int32(attempt)
	env.OwnershipBoundary = "task:" + t.Name
	env.PolicyActions = append([]string(nil), t.PolicyActions...)
	env.NestedAgentPolicy = t.NestedAgentPolicy
	if t.NestedAgentPolicy != nil {
		parent := apiv1.StagePlatformAuthority(*env, "result")
		env.ParentPlatformPolicy = &parent
	}
}
