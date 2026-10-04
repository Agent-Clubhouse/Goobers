package main

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (s *daemonCredentialService) childBlobPlane(base blobstore.Store) blobstore.Store {
	return parentBlobPlane{base: generatedBlobPlane{base: childpod.BlobOverlay{Base: base, Queue: s.childQueue, Scope: s.childBlobScope}, service: s}, service: s}
}

func (s *daemonCredentialService) childBlobScope(ctx context.Context) (triggerqueue.ChildIdentity, bool, error) {
	principal, ok := httpapi.PrincipalFromContext(ctx)
	if !ok {
		return triggerqueue.ChildIdentity{}, false, errors.New("blob custody requires an authenticated principal")
	}
	if !httpapi.IsPodPrincipal(principal) {
		return triggerqueue.ChildIdentity{}, false, nil
	}
	if principal.WorkflowParent {
		return triggerqueue.ChildIdentity{}, false, errors.New("contained parent blob custody requires its authority adapter")
	}
	runID, ok := strings.CutPrefix(principal.Subject, "run:")
	if !ok || !apiv1.ValidRunID(runID) || s.childQueue == nil {
		return triggerqueue.ChildIdentity{}, false, errors.New("blob custody run identity unavailable")
	}
	child, err := s.childQueue.ChildForExecutionRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		if principal.GeneratedChild {
			return triggerqueue.ChildIdentity{}, true, errors.New("signed child blob lineage unavailable")
		}
		// A retained journal still identifies a child after queue expiry. Missing
		// ordinary journals are legitimate for engine-driven remote runs.
		dir, findErr := s.layout.FindRunDir(runID)
		if findErr != nil && !errors.Is(findErr, fs.ErrNotExist) {
			return triggerqueue.ChildIdentity{}, false, findErr
		}
		if findErr == nil {
			reader, readErr := journal.OpenReadOnly(dir)
			if readErr != nil {
				return triggerqueue.ChildIdentity{}, false, readErr
			}
			id, readErr := reader.Identity()
			if readErr != nil || id.Child != nil {
				return triggerqueue.ChildIdentity{}, true, errors.New("child blob lineage expired or unavailable")
			}
		}
		return triggerqueue.ChildIdentity{}, false, nil
	}
	if err != nil {
		return triggerqueue.ChildIdentity{}, true, err
	}
	if !principal.GeneratedChild {
		return triggerqueue.ChildIdentity{}, true, errors.New("child blob custody requires a signed child principal")
	}
	if err := s.verifyChildBlobOwner(ctx, child, runID); err != nil {
		return triggerqueue.ChildIdentity{}, true, err
	}
	return child.Identity, true, nil
}

func (s *daemonCredentialService) verifyChildBlobOwner(ctx context.Context, child triggerqueue.ChildRecord, runID string) error {
	if child.State.Terminal() || !child.AcknowledgedAt.IsZero() || !child.TombstonedAt.IsZero() {
		return errors.New("child blob custody is closed")
	}
	dir, err := s.layout.FindRunDir(runID)
	if err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	ref, err := retainedChildExecutionRef(ctx, s.childQueue, id, false)
	if err != nil || id.RunID != runID || ref.Child.Identity != child.Identity {
		return errors.New("child blob custody differs from journal provenance")
	}
	// Cancellation prevents new execution/credentials, but the existing pod
	// still must surrender bounded output during teardown. No shared-store
	// access or new authority follows from this custody-only permission.
	return id.ValidateChildLineage()
}
