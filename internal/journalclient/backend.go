package journalclient

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// NewBackendCrossRun uses logical journal identities for live cross-run reads.
// Candidate discovery for windowed reads still requires the live read model.
func NewBackendCrossRun(backend journal.ReaderBackend) *FileCrossRun {
	return &FileCrossRun{backend: backend}
}

func (f *FileCrossRun) candidateRoots(gaggle string) ([]string, error) {
	if f.backend != nil {
		return nil, nil
	}
	return f.scoped(gaggle).RunDirs()
}

func (f *FileCrossRun) openRun(ctx context.Context, id, gaggle string) (journal.RunReader, error) {
	if !apiv1.ValidRunID(id) {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	backend := f.backend
	if backend == nil {
		backend = journal.NewFileReaderBackend(journal.FileReaderBackendConfig{
			ResolveRun: func(ctx context.Context, id string) (string, error) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				return f.scoped(gaggle).FindRunDir(id)
			},
		})
	}
	reader, err := backend.OpenRun(ctx, journal.RunRef{ID: id})
	if err != nil {
		return nil, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	if identity.RunID != id || (gaggle != "" && identity.Gaggle != gaggle) {
		return nil, fmt.Errorf("journalclient: run identity does not match requested scope")
	}
	return reader, nil
}

func (f *FileCrossRun) openCandidate(ctx context.Context, candidate runCandidate, gaggle string) (journal.RunReader, error) {
	if f.backend == nil {
		return journal.OpenRead(candidate.dir)
	}
	return f.openRun(ctx, candidate.name, gaggle)
}
