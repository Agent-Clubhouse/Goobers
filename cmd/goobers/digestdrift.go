package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runcontrol"
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
	// Superseded runs are in flight for a workflow whose launch-pinned inputs
	// the reload being reported just changed (#5898). Unlike Recoverable and
	// AtRisk, which describe standing state, this set is only what this
	// reload newly left behind.
	Superseded []string
}

// pinnedDefinitions is the hot-reloadable configuration an in-flight run pins
// at launch: its compiled workflow (task timeouts), its resolved goober
// content (goober timeouts) and its effective run controls (maxRepasses and
// the run-duration limits, merged from instance, gaggle and workflow scopes).
// Instance and repo settings live in instance.yaml, which is never
// hot-reloaded.
type pinnedDefinitions struct {
	machines      map[localscheduler.WorkflowIdentity]*workflow.Machine
	gooberDigests map[localscheduler.WorkflowIdentity]string
	runControls   map[localscheduler.WorkflowIdentity]runcontrol.Effective
}

// newPinnedDefinitions resolves each served workflow's effective run controls
// the same way a launch does, so they compare against what runs pinned. A
// workflow whose controls do not resolve is left out; such a workflow cannot
// launch, so it has no controls a run could have pinned from it.
func newPinnedDefinitions(cfg *instance.Config, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, gooberDigests map[localscheduler.WorkflowIdentity]string, repoRefs map[localscheduler.WorkflowIdentity]apiv1.RepoRef, set *instance.ConfigSet) pinnedDefinitions {
	controls := make(map[localscheduler.WorkflowIdentity]runcontrol.Effective, len(machines))
	for key, machine := range machines {
		if machine == nil {
			continue
		}
		var gaggle apiv1.Gaggle
		if configured := configuredGaggle(set, key.Gaggle); configured != nil {
			gaggle = *configured
		}
		effective, err := resolveWorkflowRunControls(cfg, repoRefs[key], gaggle, apiv1.Workflow{Spec: machine.Def.Spec})
		if err != nil {
			continue
		}
		controls[key] = effective
	}
	return pinnedDefinitions{machines: machines, gooberDigests: gooberDigests, runControls: controls}
}

// supersededWorkflows reports whether a reload from before to after changed
// anything the given in-flight run pinned at launch. Each check requires both
// that this reload changed the value and that the run's pinned value differs
// from the new one, so a run launched on the new definitions (between the
// scheduler swap and this report) is excluded, and a gaggle edit masked by a
// workflow override leaves the effective controls, and so the report,
// unchanged.
func supersededWorkflows(before, after pinnedDefinitions) func(journal.RunIdentity) bool {
	return func(run journal.RunIdentity) bool {
		key := localscheduler.WorkflowIdentity{Gaggle: run.Gaggle, Workflow: run.Workflow}
		next := machineDigest(after.machines[key])
		if machineDigest(before.machines[key]) != next && run.WorkflowDigest != next {
			return true
		}
		if before.gooberDigests[key] != after.gooberDigests[key] && run.GooberDigest != after.gooberDigests[key] {
			return true
		}
		previous, hadPrevious := before.runControls[key]
		current, hasCurrent := after.runControls[key]
		if hadPrevious == hasCurrent && previous == current {
			return false
		}
		return !hasCurrent || !pinnedRunControlsMatch(run.RunControls, current)
	}
}

// pinnedRunControlsMatch normalizes a run's pinned controls through the same
// resolution a launch uses. A legacy run without pinned controls never
// matches: it cannot show it already runs under the new values.
func pinnedRunControlsMatch(pinned *apiv1.RunControls, current runcontrol.Effective) bool {
	if pinned == nil {
		return false
	}
	effective, err := runcontrol.Resolve(*pinned, nil, nil)
	return err == nil && effective == current
}

func machineDigest(machine *workflow.Machine) string {
	if machine == nil {
		return ""
	}
	return machine.Digest()
}

func (d workflowDigestDrift) empty() bool {
	return len(d.Recoverable) == 0 && len(d.AtRisk) == 0
}

// inspectWorkflowDigestDrift classifies every non-terminal run under l against
// the currently served machines. It is best-effort by construction: a run
// directory that cannot be opened or whose identity cannot be read is skipped
// rather than failing the caller, because this report exists to inform an
// operator, never to gate a config reload or a daemon start.
func inspectWorkflowDigestDrift(l instance.Layout, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, superseded func(journal.RunIdentity) bool) (workflowDigestDrift, error) {
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
			if superseded != nil && superseded(id) {
				drift.Superseded = append(drift.Superseded, id.RunID)
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
	sort.Strings(drift.Superseded)
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

// workflowDigestDriftNotice is the daemon-log line for the runs a reload just
// superseded (#5898). A watched edit never reaches an in-flight run: each run
// executes the workflow, goober content and gaggle run controls it pinned at
// launch, so limits such as stage timeouts and maxRepasses keep their launch
// values. Without this line an applied reload is indistinguishable from one
// the running work picked up. It returns "" when the reload changed nothing
// an in-flight run pinned.
func workflowDigestDriftNotice(drift workflowDigestDrift) string {
	if len(drift.Superseded) == 0 {
		return ""
	}
	return fmt.Sprintf("config reload: definition change detected; will apply to subsequent runs only: "+
		"%d in-flight run(s) keep the definitions they launched with, including stage timeouts and maxRepasses: %v",
		len(drift.Superseded), boundRunIDs(drift.Superseded))
}

// reportWorkflowDigestDrift surfaces a drift report both on the daemon log and
// on the instance log. Neither is fatal to the reload that produced it.
func reportWorkflowDigestDrift(instanceLog *journal.InstanceLog, drift workflowDigestDrift, logf func(string, ...any)) error {
	if notice := workflowDigestDriftNotice(drift); notice != "" {
		logf("%s", notice)
	}
	return journalWorkflowDigestDrift(instanceLog, drift)
}

func boundRunIDs(ids []string) []string {
	if len(ids) <= maxReportedDriftedRuns {
		return ids
	}
	return ids[:maxReportedDriftedRuns]
}
