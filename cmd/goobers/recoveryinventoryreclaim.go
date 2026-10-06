package main

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/instanceannotations"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/recovery"
)

// recoveryRunPhaseCache remembers terminal run phases between samples. A
// terminal phase does not change, so an elevated inventory of thousands of
// entries re-reads only the journals of runs that were still live.
type recoveryRunPhaseCache struct {
	mu       sync.Mutex
	terminal map[string]journal.RunPhase
}

func (c *recoveryRunPhaseCache) phase(layout instance.Layout, runID string) (journal.RunPhase, bool) {
	c.mu.Lock()
	cached, ok := c.terminal[runID]
	c.mu.Unlock()
	if ok {
		return cached, true
	}
	dir, err := runDirFor(layout, runID)
	if err != nil {
		return "", false
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return "", false
	}
	phase, err := reader.Phase()
	if err != nil {
		return "", false
	}
	if terminalRunPhase(phase) {
		c.mu.Lock()
		if c.terminal == nil {
			c.terminal = make(map[string]journal.RunPhase)
		}
		c.terminal[runID] = phase
		c.mu.Unlock()
	}
	return phase, true
}

// retain drops cached runs no longer holding a slot, so the cache is bounded
// by the inventory rather than by the instance's history.
func (c *recoveryRunPhaseCache) retain(runIDs map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for runID := range c.terminal {
		if _, ok := runIDs[runID]; !ok {
			delete(c.terminal, runID)
		}
	}
}

type recoveryReclaimRecord struct {
	path   string
	record recovery.Record
}

// attachRecoveryReclaimGuidance turns an elevated reading into commands an
// operator can run: the oldest snapshots whose owning run is terminal (the
// only ones recovery-abandon and recovery-restore accept), and the retention
// setting, if any, that would stop an abandonment from freeing its slot.
//
// Overflow entries are deliberately not candidates: they hold no slot, and
// they are promoted back into the inventory as slots free.
func attachRecoveryReclaimGuidance(layout instance.Layout, cfg *instance.Config, phases *recoveryRunPhaseCache, status *readservice.RecoveryInventoryStatus, entries []recovery.InventoryEntry, now time.Time, goos string) {
	status.StatusCommand = operatorCommand(goos, "goobers", "status", "--all", layout.Root)
	status.ReclaimHold = recoveryReclaimHold(layout, cfg, now)

	records := make([]recoveryReclaimRecord, 0, len(entries))
	held := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			continue
		}
		records = append(records, recoveryReclaimRecord{path: entry.RecordPath, record: record})
		held[record.RunID] = struct{}{}
	}
	phases.retain(held)
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i].record, records[j].record
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		return a.Ref < b.Ref
	})

	// Unknown abandonment state only means an abandon command may be offered
	// for a snapshot already abandoned; repeating it is harmless.
	abandonments, _ := instanceannotations.ForInstance(layout.SchedulerDir()).RecoveryAbandonments(layout.SchedulerDir())
	for _, candidate := range records {
		phase, ok := phases.phase(layout, candidate.record.RunID)
		if !ok || !terminalRunPhase(phase) {
			continue
		}
		status.ReclaimCandidatesTotal++
		if len(status.ReclaimCandidates) >= readservice.RecoveryReclaimCandidateLimit {
			continue
		}
		status.ReclaimCandidates = append(status.ReclaimCandidates, recoveryReclaimCandidate(layout, candidate, phase, abandonments, now, goos))
	}
}

func recoveryReclaimCandidate(layout instance.Layout, candidate recoveryReclaimRecord, phase journal.RunPhase, abandonments []journal.Event, now time.Time, goos string) readservice.RecoveryReclaimCandidate {
	record := candidate.record
	abandoned := false
	if len(abandonments) > 0 {
		abandoned, _ = recovery.ExplicitlyAbandoned(abandonments, record)
	}
	out := readservice.RecoveryReclaimCandidate{
		RunID: record.RunID, Phase: string(phase), Ref: record.Ref, PatchDigest: record.PatchDigest,
		RepositoryKey: record.RepositoryKey, CreatedAt: record.CreatedAt.UTC(), RetainUntil: record.RetainUntil.UTC(),
		Abandoned:      abandoned,
		InspectCommand: operatorCommand(goos, "goobers", "trace", "--summary", record.RunID, layout.Root),
	}
	if now.Before(record.RetainUntil) {
		// Run from a checkout of the snapshot's repository; restore creates a
		// new local branch and leaves the checkout itself untouched.
		out.RestoreCommand = operatorCommand(goos, "goobers", "recovery-restore",
			"--record", candidate.path, "--repository", ".", "--branch", "recovered/"+record.RunID, layout.Root)
	}
	if !abandoned {
		out.AbandonCommand = operatorCommand(goos, "goobers", "recovery-abandon",
			"--run", record.RunID, "--ref", record.Ref, "--confirm-digest", record.PatchDigest, layout.Root)
	}
	return out
}

// recoveryReclaimHold reports the retention setting that keeps a pass from
// deleting abandoned and expired snapshots, in the order the pass itself
// applies them. Contentless entries are reclaimed regardless and need no
// operator action, so they do not lift a hold.
func recoveryReclaimHold(layout instance.Layout, cfg *instance.Config, now time.Time) *readservice.RecoveryReclaimHold {
	retention := instance.RetentionConfig{}
	if loaded, err := instance.LoadConfig(layout.ConfigFile()); err == nil {
		retention = loaded.Retention
	} else if cfg != nil {
		retention = cfg.Retention
	}
	hold := &readservice.RecoveryReclaimHold{ConfigFile: layout.ConfigFile()}
	switch {
	case !retention.EnabledEffective() && !retention.DryRun:
		hold.Reason, hold.Setting = readservice.RecoveryReclaimHoldDisabled, "retention.enabled: true"
	case retention.DryRun:
		hold.Reason, hold.Setting = readservice.RecoveryReclaimHoldDryRun, "retention.dryRun: false"
	default:
		state, _, err := readRetentionGraceState(layout, worktreeRetentionStateFile)
		if err != nil || !retentionPassIsDryRun(state, retention.ImmediateFirstEnable(), now) {
			return nil
		}
		hold.Reason, hold.Setting = readservice.RecoveryReclaimHoldGrace, "retention.firstEnable: immediate"
		if !state.EnforceAt.IsZero() {
			until := state.EnforceAt.UTC()
			hold.Until = &until
		}
	}
	return hold
}

var operatorCommandBareArg = regexp.MustCompile(`^[A-Za-z0-9_./:=+-]+$`)

// operatorCommand renders argv for the shell the daemon's host uses
// (PowerShell on Windows, POSIX sh elsewhere), quoting only what needs it so
// the common case stays readable.
func operatorCommand(goos string, argv ...string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if operatorCommandBareArg.MatchString(arg) {
			quoted[i] = arg
			continue
		}
		quoted[i] = quoteShellArg(arg, goos)
	}
	return strings.Join(quoted, " ")
}
