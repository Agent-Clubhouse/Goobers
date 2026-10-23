package journal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

type readerBackendFactory func() (journal.ReaderBackend, error)

func TestJournalReaderBackendConformanceRejectsBrokenBackends(t *testing.T) {
	base := fileReaderBackendFactory(t)
	for name, wrap := range map[string]func(journal.ReaderBackend) journal.ReaderBackend{
		"events-out-of-order": func(next journal.ReaderBackend) journal.ReaderBackend {
			return &outOfOrderReaderBackend{ReaderBackend: next}
		},
		"after-seq-ignored": func(next journal.ReaderBackend) journal.ReaderBackend {
			return &ignoreAfterSeqReaderBackend{ReaderBackend: next}
		},
		"run-filter-ignored": func(next journal.ReaderBackend) journal.ReaderBackend {
			return &ignoreRunFilterReaderBackend{ReaderBackend: next}
		},
		"instance-cursor-ignored": func(next journal.ReaderBackend) journal.ReaderBackend {
			return &ignoreInstanceCursorReaderBackend{ReaderBackend: next}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := journalReaderBackendConformance(func() (journal.ReaderBackend, error) {
				backend, err := base()
				if err != nil {
					return nil, err
				}
				return wrap(backend), nil
			}); err == nil {
				t.Fatal("broken reader backend passed conformance")
			}
		})
	}
}

func journalReaderBackendConformance(factory readerBackendFactory) error {
	backend, err := factory()
	if err != nil {
		return err
	}
	ctx := context.Background()
	refs, err := backend.ListRuns(ctx, journal.RunFilter{})
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if got := runRefIDs(refs); !slices.Equal(got, []string{"run-alpha", "run-reader", "run-zeta"}) {
		return fmt.Errorf("list runs = %v", got)
	}
	filtered, err := backend.ListRuns(ctx, journal.RunFilter{After: "run-alpha", Limit: 1})
	if err != nil {
		return fmt.Errorf("filtered runs: %w", err)
	}
	if got := runRefIDs(filtered); !slices.Equal(got, []string{"run-reader"}) {
		return fmt.Errorf("filtered runs = %v", got)
	}
	ref := journal.RunRef{ID: "run-reader", ReadOnly: true}
	reader, err := backend.OpenRun(ctx, ref)
	if err != nil {
		return fmt.Errorf("open run: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	if identity.RunID != ref.ID || identity.Gaggle != "g" || identity.Workflow != "implementation" {
		return fmt.Errorf("identity = %+v", identity)
	}
	records, err := backend.ReadEvents(ctx, ref, 1, 2)
	if err != nil {
		return fmt.Errorf("events after seq: %w", err)
	}
	if got := readEventIdentities(records); !slices.Equal(got, []readEventIdentity{
		{Seq: 2, Type: journal.EventRunnerAnnotation, Name: "first"},
		{Seq: 3, Type: journal.EventArtifactRecorded, Name: "blob"},
	}) {
		return fmt.Errorf("events after seq = %+v", got)
	}
	artifact := records[1].Event.Ref
	if artifact == nil {
		return errors.New("artifact event has no ref")
	}
	data, err := reader.ArtifactBytes(*artifact)
	if err != nil {
		return fmt.Errorf("artifact bytes: %w", err)
	}
	if !bytes.Equal(data, []byte("reader-blob")) {
		return fmt.Errorf("artifact bytes = %q", data)
	}
	instance, err := backend.InstanceRead(ctx, 1, 1)
	if err != nil {
		return fmt.Errorf("instance read: %w", err)
	}
	if len(instance) != 1 || instance[0].Seq != 2 || instance[0].Type != journal.EventRunnerAnnotation || instance[0].Name != "second" {
		return fmt.Errorf("instance read = %+v", instance)
	}
	if err := readerHandleConformance(ctx, backend, ref, reader); err != nil {
		return err
	}
	return readerPageContinuation(ctx, backend, ref)
}

func readerPageContinuation(ctx context.Context, backend journal.ReaderBackend, ref journal.RunRef) error {
	records, err := backend.ReadEvents(ctx, ref, 3, 1)
	if err != nil {
		return err
	}
	if len(records) != 1 || records[0].Event.Seq != 4 || records[0].Event.Name != "surplus" {
		return fmt.Errorf("continued event page = %+v", records)
	}
	events, err := backend.InstanceRead(ctx, 2, 1)
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0].Seq != 3 || events[0].Name != "third" {
		return fmt.Errorf("continued instance page = %+v", events)
	}
	events, err = backend.InstanceRead(ctx, 3, 1)
	if err != nil || len(events) != 0 {
		return fmt.Errorf("instance past end = %+v: %v", events, err)
	}
	return nil
}

func readerHandleConformance(ctx context.Context, backend journal.ReaderBackend, ref journal.RunRef, reader journal.RunReader) error {
	records, err := backend.ReadEvents(ctx, ref, 0, 0)
	if err != nil {
		return err
	}
	if len(records) != 6 {
		return fmt.Errorf("all event records = %d", len(records))
	}
	if !bytes.Equal(records[1].Raw, []byte(unknownReaderRecord)) {
		return errors.New("original raw bytes including unknown fields were changed")
	}
	handleRecords, err := reader.EventRecords()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(handleRecords, records) {
		return errors.New("handle records differ from paged records")
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	if len(events) != len(records) {
		return errors.New("handle events are incomplete")
	}
	for i, record := range records {
		if !reflect.DeepEqual(events[i], record.Event) {
			return fmt.Errorf("handle event %d differs", i)
		}
		var decoded journal.Event
		if err := json.Unmarshal(record.Raw, &decoded); err != nil {
			return fmt.Errorf("raw event %d: %w", i, err)
		}
		if !reflect.DeepEqual(decoded, record.Event) {
			return fmt.Errorf("raw event %d differs", i)
		}
	}
	state, err := reader.State()
	if err != nil {
		return err
	}
	if state.RunID != ref.ID || state.LastSeq != 6 || state.Phase != journal.PhaseCompleted || !state.KnownSchema() {
		return fmt.Errorf("handle checkpoint = %+v", state)
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseCompleted {
		return fmt.Errorf("handle phase = %s: %v", phase, err)
	}
	bounded, err := reader.PhaseBounded(ctx)
	if err != nil || bounded != phase {
		return fmt.Errorf("bounded phase = %s: %v", bounded, err)
	}
	if reader.Schema().Version != journal.CurrentSchemaVersion {
		return fmt.Errorf("handle schema = %+v", reader.Schema())
	}
	artifact := records[2].Event.Ref
	if artifact == nil {
		return errors.New("missing artifact ref")
	}
	if _, err := reader.ArtifactBytesBounded(*artifact, 1); err == nil {
		return errors.New("artifact byte ceiling ignored")
	}
	data, err := reader.ArtifactByDigest(artifact.Digest)
	if err != nil || !bytes.Equal(data, []byte("reader-blob")) {
		return fmt.Errorf("digest lookup = %q: %v", data, err)
	}
	span := records[5].Event.Ref
	if span == nil {
		return errors.New("missing span ref")
	}
	data, err = reader.SpanBytes(*span)
	if err != nil || !bytes.Equal(data, []byte("reader-span")) {
		return fmt.Errorf("span = %q: %v", data, err)
	}
	bad := *artifact
	bad.Digest = "sha256:" + string(bytes.Repeat([]byte("0"), 64))
	if _, err := reader.ArtifactBytes(bad); err == nil {
		return errors.New("invalid artifact digest accepted")
	}
	return nil
}

type readEventIdentity struct {
	Seq  uint64
	Type journal.EventType
	Name string
}

func readEventIdentities(records []journal.EventRecord) []readEventIdentity {
	result := make([]readEventIdentity, 0, len(records))
	for _, record := range records {
		result = append(result, readEventIdentity{
			Seq: record.Event.Seq, Type: record.Event.Type, Name: record.Event.Name,
		})
	}
	return result
}

func runRefIDs(refs []journal.RunRef) []string {
	result := make([]string, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref.ID)
	}
	return result
}

type outOfOrderReaderBackend struct{ journal.ReaderBackend }

func (b *outOfOrderReaderBackend) ReadEvents(ctx context.Context, ref journal.RunRef, afterSeq uint64, limit int) ([]journal.EventRecord, error) {
	records, err := b.ReaderBackend.ReadEvents(ctx, ref, afterSeq, limit)
	slices.Reverse(records)
	return records, err
}

type ignoreAfterSeqReaderBackend struct{ journal.ReaderBackend }

func (b *ignoreAfterSeqReaderBackend) ReadEvents(ctx context.Context, ref journal.RunRef, _ uint64, limit int) ([]journal.EventRecord, error) {
	return b.ReaderBackend.ReadEvents(ctx, ref, 0, limit)
}

type ignoreRunFilterReaderBackend struct{ journal.ReaderBackend }

func (b *ignoreRunFilterReaderBackend) ListRuns(ctx context.Context, _ journal.RunFilter) ([]journal.RunRef, error) {
	return b.ReaderBackend.ListRuns(ctx, journal.RunFilter{})
}

type ignoreInstanceCursorReaderBackend struct{ journal.ReaderBackend }

func (b *ignoreInstanceCursorReaderBackend) InstanceRead(ctx context.Context, _ uint64, limit int) ([]journal.Event, error) {
	return b.ReaderBackend.InstanceRead(ctx, 0, limit)
}
