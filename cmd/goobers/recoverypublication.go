package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

func (s recoveryDeliveryService) PublishRecovery(ctx context.Context, runID, key, issue string, body io.Reader) error {
	deadline, err := authorizeRecoveryDelivery(ctx, s.layout, runID, key, issue, time.Now().UTC())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if s.setup == nil {
		return fmt.Errorf("recovery publication has no managed repositories")
	}
	managers, roots, err := retentionManagers(s.layout, s.setup)
	if err != nil {
		return err
	}
	manager, runDir, err := recoveryRetentionOwner(runID, managers, roots)
	if err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil || identity.RunID != runID || identity.StartedAt.IsZero() {
		return fmt.Errorf("recovery publication run identity unavailable")
	}
	cfg, err := instance.LoadConfig(s.layout.ConfigFile())
	if err != nil {
		return err
	}
	url, err := recoveryRetentionCloneURL(cfg, key)
	if err != nil {
		return err
	}
	root, err := prepareRecoveryInventory(s.layout.Root)
	if err != nil {
		return err
	}
	err = manager.WithRecoveryMirror(ctx, url, func(repository string) error {
		_, _, err := recovery.AcceptArchive(ctx, body, recovery.RetentionRequest{
			Repository: repository, RepositoryKey: key, RunID: runID,
			IdentityTime: identity.StartedAt, RetainUntil: identity.StartedAt.Add(30 * 24 * time.Hour),
			InventoryRoot: root, CleanupRoots: []string{manager.Root}, MaxSnapshots: 128, MaxArchiveBytes: 512 << 20,
		}, recoveryPublicationAck{ctx: ctx, service: s, runID: runID, key: key, issue: issue})
		return err
	})
	return err
}

type recoveryPublicationAck struct {
	ctx               context.Context
	service           recoveryDeliveryService
	runID, key, issue string
}

func (a recoveryPublicationAck) Append(event journal.Event) error {
	_, err := withAuthorizedRecoveryDelivery(a.ctx, a.service.layout, a.runID, a.key, a.issue, time.Now().UTC(), func() error {
		return (recoveryCleanupJournal{directory: a.service.layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}).Append(event)
	})
	return err
}
