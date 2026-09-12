package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

const recoveryAbandonHelp = "Usage: goobers recovery-abandon --run <run-id> --ref <recovery-ref> --confirm-digest <patch-digest> [instance]\n\n" +
	"Abandon one exact retained snapshot from a terminal run. The digest must\n" +
	"match its published patch digest. Records an operator decision and prevents\n" +
	"automatic recovery selection. Configured retention subsequently removes\n" +
	"the owned recovery ref and archive, not user branches. Active runs, stage\n" +
	"execution, and ambiguous matches are refused.\n"

func runRecoveryAbandon(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("recovery-abandon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "recovery-abandon")
	runID := fs.String("run", "", "terminal source run")
	ref := fs.String("ref", "", "exact published recovery ref")
	digest := fs.String("confirm-digest", "", "confirm the published patch digest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *runID == "" || *ref == "" || *digest == "" || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if claimsPlaneSelected() || os.Getenv("GOOBERS_RUN_ID") != "" {
		pf(stderr, "error: recovery abandonment requires a local operator, not stage execution\n")
		return 1
	}
	layout := instance.NewLayout(providerStageRoot(fs.Arg(0)))
	if err := prepareManualRoot(layout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := abandonRecoveryRecord(ctx, layout, *runID, *ref, *digest); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "abandoned recovery run=%q ref=%q; configured retention will reap owned recovery state\n", *runID, *ref)
	return 0
}

func abandonRecoveryRecord(ctx context.Context, layout instance.Layout, runID, ref, digest string) error {
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil {
		return err
	}
	var selected *recovery.InventoryEntry
	for _, entry := range entries {
		current, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return err
		}
		if current.RunID != runID || current.Ref != ref || current.PatchDigest != digest {
			continue
		}
		if selected != nil {
			return fmt.Errorf("ambiguous recovery snapshot; abandonment refused")
		}
		entry.Record = current
		selected = &entry
	}
	if selected == nil {
		return fmt.Errorf("no retained snapshot matches the run, ref, and confirmed digest")
	}
	dir, err := runDirFor(layout, runID)
	if err != nil {
		return err
	}
	entered, err := journal.WithIdleRunReader(ctx, dir, func(reader *journal.Reader) error {
		return recordRecoveryAbandonment(ctx, layout, reader, *selected)
	})
	if err == nil && !entered {
		return fmt.Errorf("recovery source is busy; abandonment refused")
	}
	return err
}

func recordRecoveryAbandonment(ctx context.Context, layout instance.Layout, reader *journal.Reader, selected recovery.InventoryEntry) error {
	identity, err := reader.Identity()
	if err != nil || identity.RunID != selected.Record.RunID {
		return fmt.Errorf("recovery abandonment run identity mismatch")
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return err
	}
	if !terminalRunPhase(phase) {
		return fmt.Errorf("recovery abandonment requires a terminal run")
	}
	current, err := recovery.ReadRetainedRecord(selected.RecordPath)
	if err != nil {
		return err
	}
	if current != selected.Record {
		return recovery.ErrRecordConflict
	}
	event, err := recovery.AbandonedEvent(current)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		return err
	}
	appendErr := log.Append(event)
	closeErr := log.Close()
	if appendErr != nil {
		return appendErr
	}
	return closeErr
}
