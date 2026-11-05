package repair

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestRegressionBackendOnlyRepair(t *testing.T) {
	for _, projected := range []bool{false, true} {
		name := "discover"
		if projected {
			name = "preserve"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			authoritativeRoot := t.TempDir()
			id := "run-backend"
			writeRun(t, authoritativeRoot, id)
			backend := journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{authoritativeRoot}})
			// The backend is valid and has a complete run. Its storage location is not a control-plane run root.
			reader, err := backend.OpenRun(ctx, journal.RunRef{ID: id})
			if err != nil {
				t.Fatal(err)
			}
			identity, err := reader.Identity()
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
			if err != nil {
				t.Fatal(err)
			}
			if projected {
				if err = store.UpsertRun(ctx, projection); err != nil {
					t.Fatal(err)
				}
			}
			sweeper := New(store, store, nil, Options{Journal: backend, RunsDirs: []string{t.TempDir()}, BatchSize: 10, Now: func() time.Time { return time.Now().Add(time.Hour) }})
			if err = sweeper.Step(ctx); err != nil {
				t.Fatal(err)
			}
			_, found, err := store.GetRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("authoritative backend run lost: preprojected=%v stats=%+v", projected, sweeper.stats)
			}
		})
	}
}

type failingBackend struct {
	journal.ReaderBackend
	openErr  error
	cancel   context.CancelFunc
	contexts []context.Context
}

func (b *failingBackend) OpenRun(ctx context.Context, ref journal.RunRef) (journal.RunReader, error) {
	b.contexts = append(b.contexts, ctx)
	if b.cancel != nil {
		b.cancel()
	}
	if b.openErr != nil {
		return nil, b.openErr
	}
	return b.ReaderBackend.OpenRun(ctx, ref)
}
func (b *failingBackend) ListRuns(ctx context.Context, filter journal.RunFilter) ([]journal.RunRef, error) {
	b.contexts = append(b.contexts, ctx)
	return b.ReaderBackend.ListRuns(ctx, filter)
}

func TestBackendRepairPreservesRowsOnErrorAndCarriesContext(t *testing.T) {
	store := openStore(t)
	root := t.TempDir()
	writeRun(t, root, "run-backend")
	backend := &failingBackend{ReaderBackend: journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}})}
	sweeper := New(store, store, nil, Options{Journal: backend, BatchSize: 10, Now: func() time.Time { return time.Now().Add(time.Hour) }})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := sweeper.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for _, got := range backend.contexts {
		if _, ok := got.Deadline(); !ok {
			t.Fatal("backend lost deadline")
		}
	}
	backend.openErr = errors.New("backend unavailable")
	if err := sweeper.Step(ctx); err != nil {
		t.Fatalf("per-run failure stopped the sweep: %v", err)
	}
	if sweeper.Stats().Failures != 1 {
		t.Fatalf("lookup failure not counted: %+v", sweeper.Stats())
	}
	if _, found, err := store.GetRun(ctx, "run-backend"); err != nil || !found {
		t.Fatalf("projection removed on error: found=%v error=%v", found, err)
	}
}

func TestBackendRepairPropagatesCancellationDuringLookup(t *testing.T) {
	store := openStore(t)
	root := t.TempDir()
	writeRun(t, root, "run-backend")
	backend := &failingBackend{ReaderBackend: journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}})}
	sweeper := New(store, store, nil, Options{Journal: backend, BatchSize: 10, Now: func() time.Time { return time.Now().Add(time.Hour) }})
	if err := sweeper.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend.openErr, backend.cancel = context.Canceled, cancel
	if err := sweeper.Step(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup error = %v", err)
	}
	if sweeper.Stats().Failures != 0 {
		t.Fatalf("request cancellation counted as a corrupt run: %+v", sweeper.Stats())
	}
	if _, found, err := store.GetRun(t.Context(), "run-backend"); err != nil || !found {
		t.Fatalf("projection removed on cancellation: found=%v error=%v", found, err)
	}
}
