package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrRunBranchLost is the sentinel every *RunBranchLostError matches with
// errors.Is: a run's own branch that an earlier Create for the same owning
// run already had in the managed working copy is gone (#4479).
var ErrRunBranchLost = errors.New("worktree: run branch lost")

// RunBranchLostError reports that Branch, which an earlier stage of
// OwnerRunID already had in the managed working copy, no longer exists
// there. Something outside the run removed it between stages: an external
// deletion, a pruning mirror fetch (for example a pinned-workspace refresh
// over a shared repo.git, #6433), or repository maintenance.
//
// Create refuses rather than cutting a fresh branch from BaseRef. A fresh
// branch would hand the later stage a pristine base checkout under the run's
// branch name, so its empty diff would be blamed on the implementer instead
// of on the infrastructure that lost the work.
type RunBranchLostError struct {
	Branch     string
	OwnerRunID string
	RunID      string
}

func (e *RunBranchLostError) Error() string {
	return fmt.Sprintf("worktree: run branch %q for run %s was established by an earlier stage but is missing from the working copy "+
		"(deleted or pruned outside the run); refusing to recreate it from base for %s",
		e.Branch, e.OwnerRunID, e.RunID)
}

// Is makes errors.Is(err, ErrRunBranchLost) hold for every RunBranchLostError.
func (e *RunBranchLostError) Is(target error) bool {
	return target == ErrRunBranchLost
}

// runBranchEstablishment is the durable evidence that a run's branch was
// present in the managed working copy. It lives beside the run's branch
// acquisition records, so FinalizeRun removes both together.
type runBranchEstablishment struct {
	OwnerRunID string `json:"owner_run_id"`
	Branch     string `json:"branch"`
}

func (m *Manager) runBranchEstablishedPath(key, ownerRunID, branch string) string {
	return strings.TrimSuffix(m.branchAcquisitionPath(key, ownerRunID, branch), ".json") + ".established.json"
}

// recordRunBranchEstablished notes, once per owning run and branch, that the
// run's branch now exists in the working copy. Rebound branches are skipped:
// RequireExistingBranch already refuses to create them.
func (m *Manager) recordRunBranchEstablished(key string, opts CreateOptions) error {
	if opts.Branch == "" || opts.RequireExistingBranch {
		return nil
	}
	path := m.runBranchEstablishedPath(key, opts.OwnerRunID, opts.Branch)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("worktree: inspect run branch record %q for run %s: %w", opts.Branch, opts.OwnerRunID, err)
	}
	data, err := json.Marshal(runBranchEstablishment{OwnerRunID: opts.OwnerRunID, Branch: opts.Branch})
	if err != nil {
		return fmt.Errorf("worktree: encode run branch record: %w", err)
	}
	if err := writeMarkerData(path, data); err != nil {
		return fmt.Errorf("worktree: record run branch %q for run %s: %w", opts.Branch, opts.OwnerRunID, err)
	}
	return nil
}

// refuseLostRunBranch is called when opts.Branch is absent and would be
// created. It fails with *RunBranchLostError when an earlier Create for the
// same owning run recorded the branch as present. The first stage to cut the
// branch has no such record, so it proceeds exactly as before.
func (m *Manager) refuseLostRunBranch(key string, opts CreateOptions) error {
	_, err := os.Stat(m.runBranchEstablishedPath(key, opts.OwnerRunID, opts.Branch))
	switch {
	case err == nil:
		return &RunBranchLostError{Branch: opts.Branch, OwnerRunID: opts.OwnerRunID, RunID: opts.RunID}
	case os.IsNotExist(err):
		return nil
	default:
		return fmt.Errorf("worktree: inspect run branch record %q for run %s: %w", opts.Branch, opts.OwnerRunID, err)
	}
}

// RunBranchLostCode is the stable machine-readable code a lost run branch
// journals under, so telemetry and watchers can tell it apart from every
// other workspace-provisioning fault without parsing the message. Its infra_
// prefix classifies it as an infrastructure failure.
const RunBranchLostCode = "infra_run_branch_lost"

// StageErrorCode reports RunBranchLostCode to the runner's typed-cause seam.
func (e *RunBranchLostError) StageErrorCode() string {
	return RunBranchLostCode
}
