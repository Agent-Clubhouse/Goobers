package journal

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// RecordExpectedArtifactAnnotated records digest-adopted evidence only if the
// run's own scrubber preserves the expected bytes. Verification and the durable
// artifact write precede the event append under the run lock, so neither a
// mismatch nor a failed write can persist an idempotency key for missing proof.
// The caller must enforce its source-fetch size limit before calling this.
func (r *Run) RecordExpectedArtifactAnnotated(stage string, attempt int, class AttemptClass, name string, data []byte, expected Ref, integrity apiv1.Integrity, runnerMeta map[string]any) (Ref, error) {
	return r.recordExpectedArtifact(Event{
		Type: EventArtifactRecorded, Stage: stage, Attempt: attempt, AttemptClass: class,
		Name: name, Integrity: integrity, Runner: copyRunnerMeta(runnerMeta),
	}, data, 0, &expected)
}
