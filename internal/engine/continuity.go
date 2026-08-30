package engine

import (
	"fmt"
	"strings"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// continuity.go is the engine's WORKSPACE CONTINUITY RECORD (#3803, #3767):
// the ordered list of workspace-delta publications made during a run, keyed
// by the stage that produced each, and the pure selector that decides which
// entry a consuming stage continues from.
//
// Before this record the walk held ONE string — the last digest any pod
// surrendered — and threaded it only to the next pod. Two things were wrong
// with that. It was unkeyed, so nothing could say WHO a consumer was
// building on, which is what DSL 3.0's repoFrom (WF022, dsl-3.0.md §4)
// declares and what #3767 asks the runtime to enforce; and it reached only
// the pod arm, so a self-placed stage or an agentic gate after a pod stage
// saw base (#3803), and a pod after a self-placed committer was handed a
// stale digest.
//
// AUTHORITY for the runtime half of WF022 (selectDelta's refusal arm): this
// is a rule this record introduces under #3767, not one of the numbered
// rulings in delivery decisions 001-003. Decision 001 ruling 4 covers only
// the gate arm (a gate inherits its subject's repo state through the
// nil-repoFrom arm). repofrom.go's compile half classifies producers
// statically (commitsRepo, agentic on a writable repo, the ref-advancing
// builtins); the runtime record holds the stages that ACTUALLY committed —
// a worker publishes only its stage's own commits (workerhost.PublishDelta),
// a pod only base..HEAD when HEAD moved — so the refusal fires exactly when
// the last stage that really advanced the branch is one the consumer did not
// declare (the "unclassified committer" arm), and never for a stage whose
// branch moved only because syncBase merged the base in.
//
// Everything here is plain workflow state derived from the pinned spec and
// recorded activity results — deterministic under replay (architecture D8).

// continuityEntry is one publication: the winning attempt of Stage put a
// bundle carrying Base..Tip in the blob plane under Digest, produced on
// workspace Branch.
//
// Branch is the run's workspace-branch binding AS IT STOOD when the producing
// stage ran (#392): "" for a run that never rebinds — every 2.0 lane and every
// 3.0 lane but pr-remediation — and the rebound branch (a PR head) for every
// stage after the rebinding one. It is part of the KEY, not decoration: a
// bundle is a thin git bundle of base..tip, so it is only meaningful on the
// branch whose history contains its prerequisite. Handing a run-branch delta
// to a stage checked out on a PR head is not a near miss; git refuses the
// fetch, and where it would not (a shared prerequisite), applying it would
// reset the PR head onto commits from a different line of work.
type continuityEntry struct {
	Stage   string
	Attempt int
	Digest  string
	Base    string
	Tip     string
	Branch  string
}

// deltaPublication is what one stage dispatch reports back to the walk: the
// winning attempt's number and whatever it published. Digest is empty when
// the stage published nothing (not a writable workspace, or its branch did
// not move). It is an out-param of dispatchWithRetry rather than part of the
// ResultEnvelope because only the winning attempt's publication counts — a
// retried attempt's bundle describes a workspace that was thrown away.
type deltaPublication struct {
	Attempt int
	Digest  string
	Base    string
	Tip     string
	// Unchanged: a writable stage succeeded without moving its branch.
	Unchanged bool
}

// RepoHandoffUndeclaredErrorCode is the journal error code for the runtime
// half of WF022 (#3767): a 3.0 consumer would have built on commits from a
// stage its repoFrom does not declare. The run fails closed.
const RepoHandoffUndeclaredErrorCode = "repo_handoff_undeclared"

// selectDelta picks the continuity entry stage continues from, among the
// entries produced on the workspace branch the consumer will itself be
// checked out on (branch — "" for a run that never rebinds).
//
//   - repoFrom declared (DSL 3.0): the most recent entry whose producer is
//     in repoFrom ∪ {stage} — a stage's own prior attempts are continuity,
//     never a handoff edge (decision 001 rule 3). If the most recent entry
//     overall is NOT in that set, the consumer would be building on commits
//     it never declared; that is refused with a named error rather than
//     resolved by either silently dropping the undeclared commits (declared
//     fetch) or silently promoting them (last-writer).
//   - repoFrom nil (DSL 2.0 tasks, gates — decision 001/gates ruling 4): the
//     last entry, byte-identical to the pre-record behaviour.
//
// An empty record selects nothing on both arms.
//
// THE BRANCH FILTER RUNS FIRST, and it is a filter rather than a refusal
// (#392). pr-remediation rebinds the run's workspace branch to the claimed
// PR's head part-way through the lane: stages before the rebind committed on
// the run branch, stages after it are cut on the PR head. A pre-rebind entry
// is therefore not a handoff this consumer declined to declare — it is work
// on a different branch, which this consumer was never continuing from on any
// substrate (the worker's mirror gives the post-rebind stage a worktree on the
// PR head, carrying no pre-rebind run-branch commit). Filtering before the
// WF022 arm keeps the refusal aimed at what it is for — an undeclared producer
// on THIS branch — instead of firing on a producer the rebind already made
// irrelevant.
//
// Zero-declaration invariance: a run that never rebinds has "" on every entry
// and on every consumer, so the filter keeps the whole record and the two arms
// below decide exactly as they did before this parameter existed.
func selectDelta(record []continuityEntry, stage string, repoFrom []string, branch string) (continuityEntry, error) {
	record = entriesOnBranch(record, branch)
	if len(record) == 0 {
		return continuityEntry{}, nil
	}
	if len(repoFrom) == 0 {
		return record[len(record)-1], nil
	}
	declared := make(map[string]bool, len(repoFrom)+1)
	for _, p := range repoFrom {
		declared[p] = true
	}
	declared[stage] = true
	latest := record[len(record)-1]
	if !declared[latest.Stage] {
		return continuityEntry{}, fmt.Errorf(
			"engine: stage %q would build on commits from %q (attempt %d, workspace delta %s), which its repoFrom [%s] does not declare (WF022 runtime, #3767); declare the producer or take it off the writable repo workspace",
			stage, latest.Stage, latest.Attempt, latest.Digest, strings.Join(repoFrom, ", "))
	}
	for i := len(record) - 1; i >= 0; i-- {
		if declared[record[i].Stage] {
			return record[i], nil
		}
	}
	// Unreachable: latest is declared, so the loop returns at the latest.
	return latest, nil
}

// entriesOnBranch keeps the entries produced on branch, preserving order.
//
// It allocates nothing and returns the record itself when every entry already
// matches, which is every run that never rebinds — the shape the invariance
// guard cares about.
func entriesOnBranch(record []continuityEntry, branch string) []continuityEntry {
	matching := true
	for i := range record {
		if record[i].Branch != branch {
			matching = false
			break
		}
	}
	if matching {
		return record
	}
	kept := make([]continuityEntry, 0, len(record))
	for _, e := range record {
		if e.Branch == branch {
			kept = append(kept, e)
		}
	}
	return kept
}

// taskConsumesDelta reports whether a task's workspace, on the arm it will
// execute on, is one the continuity record feeds. Scratch and repo-readonly
// never receive a delta on either arm (a read-only stage reads the pinned
// base by definition); the pod arm additionally treats an undeclared mode as
// no workspace at all.
//
// The mode is Task.EffectiveWorkspace — the SAME resolution the pod dispatch
// (dispatchstage.go) and the activities provision from. A private copy here
// that read Run.Workspace alone once disagreed with them for a task-level
// `workspace: repo`: the pod was cut a writable repo workspace and handed no
// delta, the exact silent drop this record exists to remove.
func taskConsumesDelta(t apiv1.Task, remote bool) bool {
	mode := t.EffectiveWorkspace()
	if remote {
		return mode.IsWritableRepo()
	}
	return writableWorkspace(mode)
}

// selectTaskDelta applies the selector for one task dispatch, journaling the
// selection so events.jsonl names the producer a consumer built on, and
// journaling the refusal (RepoHandoffUndeclaredErrorCode) before failing the
// run closed.
func selectTaskDelta(ctx workflow.Context, t apiv1.Task, remote bool, record []continuityEntry, branch string, rec *runJournal) (continuityEntry, error) {
	if !taskConsumesDelta(t, remote) {
		return continuityEntry{}, nil
	}
	selected, err := selectDelta(record, t.Name, []string(t.RepoFrom), branch)
	if err != nil {
		rec.repoHandoffRefused(ctx, t.Name, err)
		return continuityEntry{}, err
	}
	if selected.Digest != "" {
		rec.workspaceDelta(ctx, t.Name, "", 0, journal.WorkspaceDelta{
			Action: journal.WorkspaceDeltaSelected, Producer: selected.Stage, ProducerAttempt: selected.Attempt,
			Digest: selected.Digest, BaseSHA: selected.Base, TipSHA: selected.Tip, Branch: selected.Branch,
		})
	}
	return selected, nil
}

// selectGateDelta applies the nil-repoFrom arm for an agentic gate: a gate
// inherits its subject's repo state (decision 001 on gates, ruling 4), so it
// is handed the last entry whenever its reviewer evaluates in a writable repo
// workspace.
func selectGateDelta(ctx workflow.Context, g apiv1.Gate, record []continuityEntry, branch string, rec *runJournal) continuityEntry {
	if g.Evaluator != apiv1.EvaluatorAgentic || !writableWorkspace(g.EffectiveWorkspace()) {
		return continuityEntry{}
	}
	selected, _ := selectDelta(record, g.Name, nil, branch)
	if selected.Digest != "" {
		rec.workspaceDelta(ctx, "", g.Name, 0, journal.WorkspaceDelta{
			Action: journal.WorkspaceDeltaSelected, Producer: selected.Stage, ProducerAttempt: selected.Attempt,
			Digest: selected.Digest, BaseSHA: selected.Base, TipSHA: selected.Tip, Branch: selected.Branch,
		})
	}
	return selected
}

// recordPublication appends a stage's publication to the record and journals
// it; a writable stage that reported its branch unchanged is journaled as
// such, so the absence of an entry is a recorded fact rather than a silence.
// branch is the run's workspace-branch binding as it stood while t ran — the
// walk rebinds only AFTER the stage returns, so the value threaded into the
// dispatch is the one the workspace was cut on, and recording any later
// reading would mis-key the entry.
func recordPublication(ctx workflow.Context, t apiv1.Task, pub deltaPublication, record []continuityEntry, branch string, rec *runJournal) []continuityEntry {
	if pub.Digest != "" {
		rec.workspaceDelta(ctx, t.Name, "", pub.Attempt, journal.WorkspaceDelta{
			Action: journal.WorkspaceDeltaPublished, Producer: t.Name, ProducerAttempt: pub.Attempt,
			Digest: pub.Digest, BaseSHA: pub.Base, TipSHA: pub.Tip, Branch: branch,
		})
		return append(record, continuityEntry{Stage: t.Name, Attempt: pub.Attempt, Digest: pub.Digest, Base: pub.Base, Tip: pub.Tip, Branch: branch})
	}
	if pub.Unchanged {
		rec.workspaceDelta(ctx, t.Name, "", pub.Attempt, journal.WorkspaceDelta{Action: journal.WorkspaceDeltaUnchanged, Producer: t.Name, ProducerAttempt: pub.Attempt, Branch: branch})
	}
	return record
}

// workspaceDelta appends one runner.workspace.delta event.
func (r *runJournal) workspaceDelta(ctx workflow.Context, stage, gate string, attempt int, d journal.WorkspaceDelta) {
	r.append(ctx, journal.WorkspaceDeltaEvent(stage, gate, attempt, "", d))
}

// repoHandoffRefused journals the WF022-runtime refusal before the run fails.
func (r *runJournal) repoHandoffRefused(ctx workflow.Context, stage string, err error) {
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage,
		Error: &journal.ErrorDetail{Code: RepoHandoffUndeclaredErrorCode, Message: err.Error()},
	})
}
