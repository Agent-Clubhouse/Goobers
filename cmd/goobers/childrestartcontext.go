package main

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// Prior context enters only through the host's retained continuation snapshot.
// Resolve each foreign-run pointer to an earlier execution of this exact child,
// verify its immutable lineage, then copy its bounded bytes into current scoped
// pod custody. Neither a run ID nor an artifact digest is an access grant.
func (p *childStagePod) copyContext(ctx context.Context, reader *journal.Reader, blobs blobstore.Store, pointers []apiv1.ContextPointer) error {
	if len(pointers) > 128 {
		return errors.New("child context exceeds pointer bound")
	}
	readers := map[string]*journal.Reader{p.identity.RunID: reader}
	for _, pointer := range pointers {
		if pointer.Artifact == nil {
			continue
		}
		runID := pointer.RunID
		if runID == "" {
			runID = p.identity.RunID
		}
		source := readers[runID]
		if source == nil {
			var err error
			source, err = p.priorContextReader(ctx, runID)
			if err != nil {
				return err
			}
			readers[runID] = source
		}
		if err := copyContainedPodContext(ctx, source, runID, blobs, []apiv1.ContextPointer{pointer}); err != nil {
			return err
		}
	}
	return nil
}

func (p *childStagePod) priorContextReader(ctx context.Context, runID string) (*journal.Reader, error) {
	if p.identity.Child == nil || p.identity.Child.ExecutionEpoch == 0 || p.service == nil {
		return nil, errors.New("child has no admitted prior execution context")
	}
	dir, err := p.service.layout.FindRunDir(runID)
	if err != nil {
		return nil, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, err
	}
	id, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	ref, err := retainedChildExecutionRef(ctx, p.service.childQueue, id, false)
	if err != nil {
		return nil, err
	}
	if ref.Child.Identity != p.start.Child.Identity || id.RunID != runID || id.Child.ExecutionEpoch >= p.identity.Child.ExecutionEpoch {
		return nil, errors.New("context is not an earlier execution of this child")
	}
	return reader, nil
}
