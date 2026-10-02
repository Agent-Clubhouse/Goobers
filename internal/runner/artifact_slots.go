package runner

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
)

func (tf *taskFrame) recordTaskStarted(attempt int, class journal.AttemptClass) error {
	started := taskStartedEvent(tf.t, attempt, class)
	if len(tf.t.ArtifactSlots) > 0 {
		if started.Runner == nil {
			started.Runner = map[string]any{}
		}
		started.Runner["artifactVisit"] = uint64(0)
	}
	seq, err := tf.jr.AppendWithSeq(started)
	tf.startedSeq = seq
	tf.artifactVisit = seq
	return err
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

func (r *Runner) startTaskAttemptHeartbeat(ctx context.Context, tf taskFrame, branch, attempt int, class journal.AttemptClass) (context.Context, stageHeartbeat) {
	ctx = launchreceipt.WithJournalStart(ctx, tf.jr, journal.Event{Type: journal.EventStageStarted, Stage: tf.t.Name, Branch: branch, Attempt: attempt, AttemptClass: class}, tf.startedSeq)
	return r.startStageHeartbeat(ctx, tf.jr, tf.t.Name, attempt, class)
}
