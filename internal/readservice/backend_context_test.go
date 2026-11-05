package readservice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
)

type backendContextBackend struct {
	journal.ReaderBackend
	got context.Context
}

func (b *backendContextBackend) OpenRun(c context.Context, _ journal.RunRef) (journal.RunReader, error) {
	b.got = c
	return nil, errors.New("backend stop")
}
func TestRegressionRequestContextReachesBackend(t *testing.T) {
	service, _, _ := fixtureService(t)
	b := &backendContextBackend{}
	service.sources.Journal = b
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = service.getRunUnannotated(ctx, "run-backend")
	if b.got == nil {
		t.Fatal("backend not called")
	}
	if _, ok := b.got.Deadline(); !ok {
		t.Fatal("request deadline stripped before backend OpenRun")
	}
}

type instanceBackend struct {
	journal.ReaderBackend
	events []journal.Event
}

func (b instanceBackend) InstanceRead(ctx context.Context, after uint64, limit int) ([]journal.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []journal.Event
	for _, event := range b.events {
		if event.Seq > after {
			result = append(result, event)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func TestBackendHealthAndActiveCountsIgnoreLocalStorage(t *testing.T) {
	service, _, _ := fixtureService(t)
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{RunID: "run-backend", Gaggle: "g", Workflow: "wf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service.sources.Journal = instanceBackend{ReaderBackend: journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}}), events: []journal.Event{{Seq: 1, Time: updated}}}
	health, err := service.healthUnannotated(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.Freshness.JournalUpdatedAt == nil || !health.Freshness.JournalUpdatedAt.Equal(updated) {
		t.Fatalf("freshness=%+v", health.Freshness)
	}
	counts, err := service.activeRunCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if counts[localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "wf"}] != 1 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestAttributionJournalUsesBackendContext(t *testing.T) {
	service, _, _ := fixtureService(t)
	backend := &backendContextBackend{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, _, _ = attributionObservationFromRecord(ctx, backend, service.sources.Layout, readmodel.RunRow{RunID: "run-backend"}, t.TempDir(), creditgraph.RunRecord{})
	if backend.got != ctx {
		t.Fatal("attribution did not use request context and backend")
	}
}
