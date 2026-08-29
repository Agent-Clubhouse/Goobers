package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// maxReportedDriftedRuns bounds the run-ID lists journaled for one drift
// report. The counts stay exact; only the enumeration is truncated so a large
// instance cannot write an unbounded instance-log line.
const maxReportedDriftedRuns = 50

// workflowDigestDrift is the operator-visible answer to "which in-flight runs
// are pinned to a definition the daemon no longer serves?" (#3376). Every
// workflow edit mints a new digest, and an in-flight run pinned to the old one
// is resumed from its journaled definition snapshot at the next restart — or,
// if that snapshot cannot be reconstructed (a pre-snapshot run, a corrupt or
// untrusted input, a workflow that no longer resolves at all), refused by
// WF-016 and terminated. Recoverable and AtRisk separate those two fates so
// the at-risk set is a number an operator can act on BEFORE the restart
// rather than a post-mortem.
type workflowDigestDrift struct {
	// Recoverable runs are pinned to a superseded digest but carry a valid
	// pinned definition snapshot: a restart resumes them.
	Recoverable []string
	// AtRisk runs are pinned to a superseded digest with no reconstructable
	// definition: a restart refuses and fails them.
	AtRisk []string
}

func (d workflowDigestDrift) empty() bool {
	return len(d.Recoverable) == 0 && len(d.AtRisk) == 0
}

// inspectWorkflowDigestDrift classifies every non-terminal run under l against
// the currently served machines. It is best-effort by construction: a run
// directory that cannot be opened or whose identity cannot be read is skipped
// rather than failing the caller, because this report exists to inform an
// operator, never to gate a config reload or a daemon start.
func inspectWorkflowDigestDrift(l instance.Layout, machines map[localscheduler.WorkflowIdentity]*workflow.Machine) (workflowDigestDrift, error) {
	var drift workflowDigestDrift
	runDirs, err := l.RunDirs()
	if err != nil {
		return drift, err
	}
	for _, runsDir := range runDirs {
		entries, exists, err := readDirectory(runsDir)
		if !exists {
			continue
		}
		if err != nil {
			return drift, fmt.Errorf("read runs directory: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			rd, err := journal.OpenRead(filepath.Join(runsDir, e.Name()))
			if err != nil {
				if errors.Is(err, journal.ErrNotRunDirectory) {
					continue
				}
				return drift, fmt.Errorf("open run journal %q: %w", e.Name(), err)
			}
			id, err := rd.Identity()
			if err != nil {
				continue
			}
			phase, err := rd.Phase()
			if err != nil || phase != journal.PhaseRunning {
				continue
			}
			if id.WorkflowDigest == "" {
				drift.AtRisk = append(drift.AtRisk, id.RunID)
				continue
			}
			machine, ok := machines[localscheduler.WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}]
			if !ok {
				drift.AtRisk = append(drift.AtRisk, id.RunID)
				continue
			}
			if machine.Digest() == id.WorkflowDigest {
				continue
			}
			if _, err := runner.PinnedWorkflowMachine(rd, id); err != nil {
				drift.AtRisk = append(drift.AtRisk, id.RunID)
				continue
			}
			drift.Recoverable = append(drift.Recoverable, id.RunID)
		}
	}
	sort.Strings(drift.Recoverable)
	sort.Strings(drift.AtRisk)
	return drift, nil
}

// journalWorkflowDigestDrift records one drift report on the instance log.
// Nothing is written when no in-flight run is pinned to a superseded digest —
// the common case — so the log stays quiet until an edit actually strands
// work.
func journalWorkflowDigestDrift(log *journal.InstanceLog, drift workflowDigestDrift) error {
	if log == nil || drift.empty() {
		return nil
	}
	return log.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"kind":             journal.RunnerAnnotationWorkflowDigestDrift,
			"recoverableCount": len(drift.Recoverable),
			"atRiskCount":      len(drift.AtRisk),
			"recoverableRuns":  boundRunIDs(drift.Recoverable),
			"atRiskRuns":       boundRunIDs(drift.AtRisk),
		},
	})
}

func boundRunIDs(ids []string) []string {
	if len(ids) <= maxReportedDriftedRuns {
		return ids
	}
	return ids[:maxReportedDriftedRuns]
}
