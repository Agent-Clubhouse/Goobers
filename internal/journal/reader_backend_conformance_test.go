package journal_test

import (
	"bytes"
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

type readerBackendFactory func() (journal.ReaderBackend, error)

func journalReaderBackendConformance(factory readerBackendFactory) error {
	backend, err := factory()
	if err != nil {
		return err
	}
	ctx := context.Background()
	refs, err := backend.ListRuns(ctx, journal.RunFilter{Limit: 10})
	if err != nil || len(refs) != 1 || refs[0].ID != "run-reader" {
		return fmt.Errorf("list runs = %+v, %w", refs, err)
	}
	reader, err := backend.OpenRun(ctx, refs[0])
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil || identity.RunID != refs[0].ID {
		return fmt.Errorf("identity = %+v, %w", identity, err)
	}
	records, err := backend.ReadEvents(ctx, refs[0], 1, 2)
	if err != nil || len(records) != 2 || records[0].Event.Seq != 2 || records[1].Event.Seq != 3 {
		return fmt.Errorf("events after seq = %+v, %w", records, err)
	}
	artifact := records[1].Event.Ref
	if artifact == nil {
		return fmt.Errorf("artifact event has no ref")
	}
	if data, err := reader.ArtifactBytes(*artifact); err != nil || !bytes.Equal(data, []byte("reader-blob")) {
		return fmt.Errorf("artifact bytes = %q, %w", data, err)
	}
	instance, err := backend.InstanceRead(ctx, 0, 1)
	if err != nil || len(instance) != 1 || instance[0].Seq != 1 {
		return fmt.Errorf("instance read = %+v, %w", instance, err)
	}
	return nil
}
