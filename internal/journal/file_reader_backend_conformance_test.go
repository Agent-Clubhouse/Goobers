package journal_test

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func fileReaderBackendFactory(t *testing.T) readerBackendFactory {
	t.Helper()
	return func() (journal.ReaderBackend, error) {
		root := t.TempDir()
		config := journal.FileBackendConfig{
			RunsDir: filepath.Join(root, "runs"), SchedulerDir: filepath.Join(root, "scheduler"),
		}
		writer := newCleanupBackend(t, journal.NewFileBackend(config))
		ctx := context.Background()
		for _, runID := range []string{"run-alpha", "run-zeta"} {
			empty, emptyOwner, err := writer.CreateRun(ctx, journal.CreateRunRequest{
				Identity: journal.RunIdentity{RunID: runID, Gaggle: "g", Workflow: "implementation"},
			})
			if err != nil {
				return nil, err
			}
			if err := writer.Retire(ctx, empty, emptyOwner); err != nil {
				return nil, err
			}
		}
		handle, owner, err := writer.CreateRun(ctx, journal.CreateRunRequest{
			Identity: journal.RunIdentity{RunID: "run-reader", Gaggle: "g", Workflow: "implementation"},
		})
		if err != nil {
			return nil, err
		}
		if _, err := writer.AppendBatch(ctx, handle, owner, []journal.Event{
			{Type: journal.EventRunnerAnnotation, Name: "first"},
		}); err != nil {
			return nil, err
		}
		ref, err := writer.RecordBlob(ctx, handle, owner, journal.BlobRecord{
			Event: journal.Event{Type: journal.EventArtifactRecorded, Name: "blob", Integrity: apiv1.IntegrityDerived},
			Data:  []byte("reader-blob"),
		})
		if err != nil {
			return nil, err
		}
		_ = ref
		if err := writer.Retire(ctx, handle, owner); err != nil {
			return nil, err
		}
		instance, instanceOwner, _, err := writer.OpenInstance(ctx)
		if err != nil {
			return nil, err
		}
		if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventDaemonStarted}); err != nil {
			return nil, err
		}
		if err := writer.InstanceAppend(ctx, instance, instanceOwner, journal.Event{Type: journal.EventRunnerAnnotation, Name: "second"}); err != nil {
			return nil, err
		}
		if err := writer.RetireInstance(ctx, instance, instanceOwner); err != nil {
			return nil, err
		}
		return journal.NewFileReaderBackend(journal.FileReaderBackendConfig{
			RunsDirs: []string{config.RunsDir}, SchedulerDir: config.SchedulerDir,
		}), nil
	}
}
