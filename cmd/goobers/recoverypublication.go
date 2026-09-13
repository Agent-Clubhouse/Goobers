package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
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
	recoveryCfg := cfg.Retention.RecoveryEffective()
	retainWindow, err := recoveryCfg.RetainWindowEffective()
	if err != nil {
		return err
	}
	if s.setup.Telemetry != nil {
		ctx = recovery.WithSnapshotObserver(ctx, s.setup.Telemetry)
	}
	err = manager.WithRecoveryMirror(ctx, url, func(repository string) error {
		_, _, err := recovery.AcceptArchive(ctx, body, recovery.RetentionRequest{
			Repository: repository, RepositoryKey: key, RunID: runID,
			IdentityTime: identity.StartedAt, RetainUntil: identity.StartedAt.Add(retainWindow),
			InventoryRoot: root, CleanupRoots: []string{manager.Root},
			MaxSnapshots: recoveryCfg.MaxSnapshotsEffective(), MaxArchiveBytes: recoveryCfg.MaxArchiveBytesEffective(),
			EvictFull: recoveryEvictFunc(s.layout, cfg, manager, key),
		}, recoveryPublicationAck{ctx: ctx, service: s, runID: runID, key: key, issue: issue, runDir: runDir, recoveryConfig: recoveryCfg})
		return err
	})
	return err
}

type recoveryPublicationAck struct {
	ctx               context.Context
	service           recoveryDeliveryService
	runID, key, issue string
	runDir            string
	recoveryConfig    instance.RecoverySnapshotConfig
}

func (a recoveryPublicationAck) Append(event journal.Event) error {
	// Publication precedes this phase read. If terminal cleanup scanned before
	// publication, we observe its durable finish here. If it finishes later, its
	// inventory scan sees this archive. Archive verification/renewal stays outside
	// the short claim-lock acknowledgement section.
	event, err := a.withCurrentRetention(event)
	if err != nil {
		return err
	}
	_, err = withAuthorizedRecoveryDelivery(a.ctx, a.service.layout, a.runID, a.key, a.issue, time.Now().UTC(), func() error {
		return (recoveryCleanupJournal{directory: a.service.layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}).Append(event)
	})
	return err
}

func (a recoveryPublicationAck) withCurrentRetention(event journal.Event) (journal.Event, error) {
	reader, err := journal.OpenReadOnly(a.runDir)
	if err != nil {
		return journal.Event{}, err
	}
	identity, err := reader.Identity()
	if err != nil || identity.RunID != a.runID || identity.StartedAt.IsZero() {
		return journal.Event{}, fmt.Errorf("publication run identity changed")
	}
	if reserved, err := journal.PruneReserved(a.runDir); err != nil || reserved {
		return journal.Event{}, fmt.Errorf("publication run unavailable for retention")
	}
	retainWindow, err := a.recoveryConfig.RetainWindowEffective()
	if err != nil {
		return journal.Event{}, err
	}
	captureAt, err := recoveryCaptureTime(a.ctx, reader, identity.StartedAt, true)
	if err != nil {
		return journal.Event{}, err
	}
	records, err := recovery.RecordsFromEvents([]journal.Event{event}, a.runID)
	if err != nil || len(records) != 1 || records[0].RepositoryKey != a.key {
		return journal.Event{}, fmt.Errorf("publication acknowledgement identity mismatch")
	}
	entries, err := recovery.ReadInventory(a.ctx, filepath.Join(a.service.layout.Root, "recovery"), a.recoveryConfig.MaxSnapshotsEffective())
	if err != nil {
		return journal.Event{}, err
	}
	for _, entry := range entries {
		if entry.Record.RunID != a.runID || entry.Record.RepositoryKey != a.key || entry.Record.Ref != records[0].Ref {
			continue
		}
		comparison := entry.Record
		comparison.RetainUntil = records[0].RetainUntil
		if comparison != records[0] {
			return journal.Event{}, recovery.ErrRecordConflict
		}
		record, err := recovery.RenewRetention(a.ctx, entry.RecordPath, captureAt.Add(retainWindow), a.recoveryConfig.MaxArchiveBytesEffective())
		if err != nil {
			return journal.Event{}, err
		}
		updated, err := recovery.RetainedEvent(record)
		if err != nil {
			return journal.Event{}, err
		}
		updated.Runner["recoveryCapture"] = true
		return updated, nil
	}
	return journal.Event{}, fmt.Errorf("publication archive missing before acknowledgement")
}
