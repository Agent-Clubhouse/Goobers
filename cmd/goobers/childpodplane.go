package main

import (
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/livejournal"
)

// installChildPodPlane binds the contained backend and its narrow HTTP owners
// together before the daemon starts serving workers. Missing execution
// dependencies leave accepted children queued; no local executor is substituted.
func (s *daemonCredentialService) installChildPodPlane(client childpod.TemporalClient, surrenders dispatcher.SurrenderPlane, journals *livejournal.Writer, claims httpapi.ChildExecutionObserver, blobs blobstore.Store) []httpapi.HandlerOption {
	s.installChildPodFactories(client, surrenders, journals)
	opts := []httpapi.HandlerOption{
		httpapi.WithGeneratedChildBlobService(s.childBlobPlane(blobs)),
		httpapi.WithWorkflowParentBlobService(s.childBlobPlane(blobs)),
		httpapi.WithWorkflowParentCredentialService(parentCredentialPlane{service: s}),
		httpapi.WithWorkflowParentExecutionObserver(parentExecutionPlane{service: s, claims: claims}),
		httpapi.WithGeneratedChildCredentialService(childCredentialPlane{service: s}),
		httpapi.WithGeneratedChildExecutionObserver(childExecutionPlane{service: s, claims: claims}),
		httpapi.WithGeneratedChildSurrenderService(childSurrenderPlane{store: surrenders, service: s}),
		httpapi.WithWorkflowParentSurrenderService(parentSurrenderPlane{store: surrenders, service: s}),
	}
	if journals != nil {
		opts = append(opts, httpapi.WithGeneratedChildJournalService(childJournalPlane{writer: journals, service: s}), httpapi.WithWorkflowParentJournalService(parentJournalPlane{writer: journals, service: s}))
	}
	return opts
}
