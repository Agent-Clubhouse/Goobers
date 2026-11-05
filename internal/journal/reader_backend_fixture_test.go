package journal_test

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// seedReaderBackend writes through the public writer contract. File and PG
// factories reopen the same store for reads after every writer is retired.
func seedReaderBackend(writer journal.Backend) error {
	ctx := context.Background()
	for _, runID := range []string{"run-alpha", "run-zeta"} {
		empty, emptyOwner, err := writer.CreateRun(ctx, journal.CreateRunRequest{
			Identity: journal.RunIdentity{RunID: runID, Gaggle: "g", Workflow: "implementation"},
		})
		if err != nil {
			return err
		}
		if err := writer.Retire(ctx, empty, emptyOwner); err != nil {
			return err
		}
	}
	handle, owner, err := writer.CreateRun(ctx, journal.CreateRunRequest{
		Identity: journal.RunIdentity{RunID: "run-reader", Gaggle: "g", Workflow: "implementation"},
	})
	if err != nil {
		return err
	}
	if _, err := writer.AppendBatch(ctx, handle, owner, []journal.Event{
		{Type: journal.EventRunnerAnnotation, Name: "first"},
	}); err != nil {
		return err
	}
	ref, err := writer.RecordBlob(ctx, handle, owner, journal.BlobRecord{
		Event: journal.Event{Type: journal.EventArtifactRecorded, Name: "blob", Integrity: apiv1.IntegrityDerived},
		Data:  []byte("reader-blob"),
	})
	if err != nil {
		return err
	}
	_ = ref
	if _, err := writer.AppendBatch(ctx, handle, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Name: "surplus"}, {Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}}); err != nil {
		return err
	}
	if _, err := writer.RecordSpan(ctx, handle, owner, journal.SpanRecord{Stage: "implementation", Name: "trace", DataSchema: "text/plain", Data: []byte("reader-span")}); err != nil {
		return err
	}
	if err := writer.Checkpoint(ctx, handle, owner); err != nil {
		return err
	}
	if err := writer.Retire(ctx, handle, owner); err != nil {
		return err
	}
	instance, instanceOwner, _, err := writer.OpenInstance(ctx)
	if err != nil {
		return err
	}
	if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventDaemonStarted}); err != nil {
		return err
	}
	if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventRunnerAnnotation, Name: "second"}); err != nil {
		return err
	}
	if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventRunnerAnnotation, Name: "third"}); err != nil {
		return err
	}
	if err := writer.RetireInstance(ctx, instance, instanceOwner); err != nil {
		return err
	}
	return nil
}
