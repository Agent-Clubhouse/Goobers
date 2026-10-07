package runner

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// recoverChildTaskContext distinguishes a durable wait, the short window after
// its continued marker, and an interrupted replacement invocation. In all three
// cases the exact held checkout is adopted instead of normal Create/reset.
func recoverChildTaskContext(events []journal.Event, stage string) (*resumeContext, bool) {
	pending, marker, err := pendingChildWait(events)
	if err != nil {
		return &resumeContext{stage: stage, childWaitErr: err}, true
	}
	if pending != nil {
		return childResumeContext(stage, marker, pending, nil, journal.Event{}), true
	}
	var started, wait journal.Event
	var record *childWaitRecord
	var pointer *apiv1.ContextPointer
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			started = event
		case journal.EventStageFinished, journal.EventRunFinished:
			record, pointer = nil, nil
		case journal.EventRunnerAnnotation:
			switch event.Runner["kind"] {
			case ChildWaitKind:
				record, err = decodeChildWaitEvent(event, started)
				if err != nil {
					return &resumeContext{stage: stage, childWaitErr: err}, true
				}
				wait, pointer = event, nil
			case ChildContinuedKind:
				if record == nil {
					continue
				}
				pointer, err = childContinuationPointer(event)
				if err != nil {
					return &resumeContext{stage: stage, childWaitErr: err}, true
				}
			}
		}
	}
	if record == nil || pointer == nil {
		return nil, false
	}
	ctx := childResumeContext(stage, wait, record, pointer, started)
	if ctx.childWaitRunning {
		ctx.policyAttempts = policyAttemptsBefore(events, stage, ctx.attempt)
		ctx.infrastructureFailures = infrastructureFailuresBefore(events, stage, ctx.attempt)
		ctx.committedWorkOnInfra = infraFailedAttemptCommittedWork(events, stage, ctx.attempt)
		ctx.mutated = interruptedAttemptMutated(events, stage, ctx.attempt)
	}
	return ctx, true
}

func childResumeContext(stage string, marker journal.Event, record *childWaitRecord, pointer *apiv1.ContextPointer, started journal.Event) *resumeContext {
	ctx := &resumeContext{stage: stage, attempt: marker.Attempt, class: marker.AttemptClass, childWait: record, childWaitCompletion: pointer}
	if marker.Stage != stage {
		ctx.childWaitErr = fmt.Errorf("runner: child wait stage disagrees with checkpoint")
		return ctx
	}
	if pointer != nil && started.Seq > marker.Seq {
		if started.Stage != stage {
			ctx.childWaitErr = fmt.Errorf("runner: child continuation changed stage before settlement")
			return ctx
		}
		ctx.attempt, ctx.class, ctx.childWaitRunning = started.Attempt, started.AttemptClass, true
	}
	return ctx
}

func childContinuationPointer(event journal.Event) (*apiv1.ContextPointer, error) {
	data, err := json.Marshal(event.Runner["context"])
	if err != nil || len(data) > 4096 {
		return nil, fmt.Errorf("runner: invalid child continuation context")
	}
	var pointer apiv1.ContextPointer
	if err := json.Unmarshal(data, &pointer); err != nil || pointer.Name != "child-completion" || pointer.RunID != "" || pointer.External != nil || pointer.Artifact == nil {
		return nil, fmt.Errorf("runner: child continuation lost its owned receipt")
	}
	if !blobstore.ValidDigest(pointer.Artifact.Digest) || pointer.Artifact.Size > maxChildWaitBytes || pointer.Artifact.Integrity != apiv1.IntegrityUnapproved || pointer.Integrity != apiv1.IntegrityUnapproved {
		return nil, fmt.Errorf("runner: child completion has invalid provenance")
	}
	return &pointer, nil
}

func childYieldedAttempts(events []journal.Event, stage string) map[int]bool {
	yielded := make(map[int]bool)
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Stage == stage && event.Runner["kind"] == ChildWaitKind {
			yielded[event.Attempt] = true
		}
	}
	return yielded
}
