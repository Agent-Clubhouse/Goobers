package journal_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

type backendIgnoreLimits struct{ journal.ReaderBackend }

func (b backendIgnoreLimits) ReadEvents(c context.Context, r journal.RunRef, a uint64, _ int) ([]journal.EventRecord, error) {
	return b.ReaderBackend.ReadEvents(c, r, a, 0)
}
func (b backendIgnoreLimits) InstanceRead(c context.Context, a uint64, _ int) ([]journal.Event, error) {
	return b.ReaderBackend.InstanceRead(c, a, 0)
}

type backendNoRaw struct{ journal.ReaderBackend }

func (b backendNoRaw) ReadEvents(c context.Context, r journal.RunRef, a uint64, l int) ([]journal.EventRecord, error) {
	v, e := b.ReaderBackend.ReadEvents(c, r, a, l)
	for i := range v {
		v[i].Raw = nil
	}
	return v, e
}

type backendEmptyHandle struct{ journal.ReaderBackend }

func (b backendEmptyHandle) OpenRun(c context.Context, r journal.RunRef) (journal.RunReader, error) {
	v, e := b.ReaderBackend.OpenRun(c, r)
	return backendEmptyReader{v}, e
}

type backendEmptyReader struct{ journal.RunReader }

func (b backendEmptyReader) Events() ([]journal.Event, error)             { return nil, nil }
func (b backendEmptyReader) EventRecords() ([]journal.EventRecord, error) { return nil, nil }
func (b backendEmptyReader) State() (journal.State, error)                { return journal.State{}, nil }
func TestRegressionReadConformanceRejectsBroken(t *testing.T) {
	for name, wrap := range map[string]func(journal.ReaderBackend) journal.ReaderBackend{
		"ignores-event-and-instance-limits": func(b journal.ReaderBackend) journal.ReaderBackend { return backendIgnoreLimits{b} },
		"reserializes-raw-records":          func(b journal.ReaderBackend) journal.ReaderBackend { return backendReserializedRaw{b} },
		"drops-raw-records":                 func(b journal.ReaderBackend) journal.ReaderBackend { return backendNoRaw{b} },
		"empty-handle-events-and-state":     func(b journal.ReaderBackend) journal.ReaderBackend { return backendEmptyHandle{b} },
	} {
		t.Run(name, func(t *testing.T) {
			factory := fileReaderBackendFactory(t)
			err := journalReaderBackendConformance(func() (journal.ReaderBackend, error) { b, e := factory(); return wrap(b), e })
			if err == nil {
				t.Fatal("incorrect reader passed shipped conformance")
			}
		})
	}
}

type backendReserializedRaw struct{ journal.ReaderBackend }

func (b backendReserializedRaw) ReadEvents(ctx context.Context, ref journal.RunRef, after uint64, limit int) ([]journal.EventRecord, error) {
	records, err := b.ReaderBackend.ReadEvents(ctx, ref, after, limit)
	if err != nil {
		return nil, err
	}
	for i := range records {
		raw, err := json.Marshal(records[i].Event)
		if err != nil {
			return nil, err
		}
		records[i].Raw = raw
	}
	return records, nil
}
