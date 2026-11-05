package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/projectorpg"
	"github.com/goobers/goobers/internal/livejournal"
)

// cloudJournalRunsRoot is the runs-directory resolver result the live writer
// requires. The PostgreSQL backend never touches it: runs live in journal.*, and
// the PG live-journal integration tests assert that no local file appears.
const cloudJournalRunsRoot = "/nonexistent/goobers-cloud-journal"

// constructJournal composes the journal plane: the live writer over the fenced
// PostgreSQL journal backend. Journal appends are lease-fenced controller
// writes even when an API request initiates them, so the seam is built from the
// controller database (internal/cloud/schema/writepaths.yaml).
//
// The object store is required, not optional: without a span source livejournal
// degrades every pod-executed agentic stage to a span_unavailable event.
func constructJournal(_ context.Context, db databaseServices, _ controlAuth, manager *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	if db.Pools == nil || db.InstanceID == "" {
		return errors.New("control journal requires the controller database and instance")
	}
	if manager == nil || owner == nil {
		return errors.New("control journal requires the controller lease")
	}
	if db.Blobs == nil {
		return errors.New("control journal requires the object store (--object-store-url): without a span source every pod-executed agentic stage would record span_unavailable")
	}
	backend, err := journalpg.New(journalpg.Config{Pool: db.Pools.Transaction, InstanceID: db.InstanceID, Lease: manager, LiveOwner: owner})
	if err != nil {
		return err
	}
	// The projector keeps the read model current from committed history; each
	// commit only nudges it (best effort), and its catch-up pass is what
	// guarantees convergence.
	wake := make(chan struct{}, 1)
	options := []livejournal.Option{livejournal.WithBackend(backend), livejournal.WithSpanSource(db.Blobs), livejournal.WithContextObserver(func(context.Context, string, uint64) {
		select {
		case wake <- struct{}{}:
		default:
		}
	})}
	if bounded, ok := db.Blobs.(livejournal.ArtifactSource); ok {
		// Prepared artifact sets carry content references; a store that cannot
		// bound reads must not accept them.
		options = append(options, livejournal.WithArtifactSource(bounded))
	}
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return cloudJournalRunsRoot, true }, options...)
	if err != nil {
		return err
	}
	p.Journal = writer
	p.journalBackend = backend
	p.journalReader, p.projectorWake, p.projectorHealth = backend, wake, &projectorpg.Health{}
	p.closers = append(p.closers, writer.Close)
	return nil
}
