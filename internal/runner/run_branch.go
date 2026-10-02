package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// WorkspaceBranchOutput is the well-known stage output that REBINDS the branch
// every subsequent stage's worktree checks out, for the rest of the run
// (issue #392).
//
// By default a run's stages share one branch — providers.BranchName,
// "goobers/<workflow>/<run-id>" — created off RepoRef.Branch by the first
// stage and checked out as-is, carrying prior stages' commits, by every stage
// after it (internal/worktree.Manager.Create's own doc comment, #133). That
// shared branch, not a shared directory, is what makes local-ci and the
// reviewer gate evaluate the run's actual diff.
//
// A workflow that re-enters on work that ALREADY has a branch needs that same
// continuity mechanism pointed somewhere else. pr-remediation is the case this
// exists for (docs/design/v0/pr-lifecycle-loop.md §5): it rebases and reworks
// an EXISTING PR, so its implement/review/local-ci stages must operate on the
// PR's own head branch, not a fresh one cut from main. Its gather-pr-context
// entrypoint emits the selected PR's head branch under this key, and every
// stage from there on — including agentic stages and gate evaluators, which
// have no way to re-checkout anything for themselves — is provisioned against
// it with no per-stage cooperation at all.
//
// The rebinding is sticky for the remainder of the run and survives a crash:
// stage outputs are journaled on stage.finished, so Resume recovers the most
// recent binding (lastWorkspaceBranch) into walkState rather than silently
// reverting a resumed run to the default branch mid-chain.
//
// A rebound branch must already exist (worktree.CreateOptions.RequireExistingBranch
// below turns a missing one into a loud failure rather than a silent empty branch
// cut from base), and must live in the run-branch namespace the worktree manager
// protects from its prune-fetch (internal/worktree/manager.go's
// runBranchNamespace) — otherwise the next stage's WorkingCopy refresh would
// delete it. Emitting an empty value is a no-op, not a reset.
//
// ONLY A DETERMINISTIC STAGE MAY REBIND. An agentic stage's Outputs are
// authored by the model itself (internal/harness's result-shape hint invites a
// free-form `"outputs": {...}` map and internal/harness.Executor passes it
// through verbatim), so honoring this key from one would let any goober, in any
// workflow, silently move every subsequent stage — including `push-branch` —
// onto a branch of its choosing. `implementation` needs no diff at all to be
// affected by that; it just needs an implementer that decides to report a
// branch name. Rebinding is a runner control-plane decision, so it is sourced
// only from stages whose output the runner itself produced.
const WorkspaceBranchOutput = "workspaceBranch"

// branchNamespaceFor resolves the run-branch namespace root for gaggle, from
// Config.BranchNamespaces, falling back to providers.DefaultBranchNamespace so
// a gaggle with no configured override (the common case) gets the historical
// "goobers/" prefix. The result is normalized to a single trailing "/", so
// callers can concatenate or prefix-match against it uniformly.
func (r *Runner) branchNamespaceFor(gaggle string) string {
	return providers.NormalizeBranchNamespace(r.cfg.BranchNamespaces[gaggle])
}

func (r *Runner) recordRunBranch(jr journalAppender, in StartInput) error {
	branch := in.WorkspaceBranch
	if branch == "" {
		branch = providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	}
	return jr.Append(journal.Event{
		Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{
			Provider:  string(in.RepoRef.Provider),
			Kind:      "branch",
			ID:        branch,
			CommitSHA: in.WorkspaceBranchSHA,
		},
	})
}

// reboundBranchBoundSHA resolves the commit this stage's workspace was checked
// out at, for a workspace bound to an existing branch. It must be read BEFORE
// the stage runs: after it, the branch may carry the stage's own commits, and
// the whole point of the value is to tell those apart from the pull request's
// pre-existing ones.
//
// An unreadable HEAD reports "" rather than an error. The bound commit only
// ever narrows what terminal capture protects, so not knowing it must fall
// back to protecting the branch, never to skipping it.
func reboundBranchBoundSHA(ctx context.Context, tf taskFrame, workspace *stageWorkspace) string {
	if tf.workspaceBranch == "" || workspace == nil || workspace.worktree == nil ||
		workspace.worktree.Branch != tf.workspaceBranch {
		return ""
	}
	sha, err := workspace.worktree.HeadSHA(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sha)
}

// ReboundWorkspaceBranchAnnotation names the runner.annotation event that
// records a stage workspace bound to an EXISTING branch (WorkspaceBranchOutput,
// the pr-remediation case). The run's own ref.touched{kind:branch} names the
// nominal run branch, which a rebound run never advances and terminal branch
// cleanup deletes as unnecessary; the commits land here instead. Terminal
// recovery capture reads this annotation so a run that fails before pushing
// still leaves a retained record of the branch it actually committed on
// (#5399). The branch name is carried under WorkspaceBranchOutput and the
// repository under "repository", as the canonical key a recovery record
// identifies its repository by.
//
// Deliberately NOT a second ref.touched{kind:branch}: that shape is the run's
// nominal branch everywhere it is read — terminal branch cleanup resolves the
// branch to DELETE from it — and a PR's own branch must never become a
// deletion candidate.
const ReboundWorkspaceBranchAnnotation = "workspace.branch.rebound"

// reboundBranchRepository is the Runner-map key carrying the repository
// identity a rebound branch lives in.
const reboundBranchRepository = "repository"

// ReboundBranchBoundSHAKey is the Runner-map key carrying the commit a rebound
// branch was at when this run's first worktree was created on it. Terminal
// recovery capture needs it to tell the run's own commits from the pull
// request's existing ones: a rebound branch is ahead of base before this run
// touches it, so "ahead of base" alone would publish a record of someone
// else's work for every run that merely bound to a pull request and failed.
const ReboundBranchBoundSHAKey = "boundSha"

// recordReboundWorkspaceBranch journals branch as a branch this run's
// worktrees were created on. Called from the stage-dispatch teardown, which is
// the only place that knows a worktree was really created on the rebound
// branch, and gated there by the same "this attempt did something" rule the
// nominal branch recording uses, so a run that rebinds and then fails before
// provisioning anything records nothing.
func (r *Runner) recordReboundWorkspaceBranch(jr journalAppender, in StartInput, branch, boundSHA string) error {
	repository := providers.RepositoryRef{
		Provider: providers.ProviderKind(in.RepoRef.Provider), URL: in.RepoRef.BaseURL,
		Owner: in.RepoRef.Owner, Project: in.RepoRef.Project, Name: in.RepoRef.Name,
	}
	return jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"annotation":             ReboundWorkspaceBranchAnnotation,
			WorkspaceBranchOutput:    branch,
			reboundBranchRepository:  repository.CanonicalKey(),
			ReboundBranchBoundSHAKey: boundSHA,
		},
	})
}

// journalWorkspaceBranches records the branches this stage's workspace was
// created on, once the attempt is known to have done something (acted): the
// run's own branch reference, still deferred until a stage really provisioned
// a worktree — provenance a schedule- or item-triggered run cannot establish
// up front — and the rebound branch the run was moved onto, if any.
func (r *Runner) journalWorkspaceBranches(ctx context.Context, jr journalAppender, tf taskFrame, workspace *stageWorkspace, boundSHA string, acted bool) error {
	in := tf.in
	if workspace.worktree == nil || workspace.worktree.Branch == "" || !machineUsesRepo(in.Machine) || !acted {
		return nil
	}
	var errs error
	if !*tf.branchRecorded {
		errs = r.journalRunBranch(ctx, jr, in, workspace.worktree)
		if errs == nil {
			*tf.branchRecorded = true
		}
	}
	return errors.Join(errs, r.journalReboundBranch(jr, in, tf, workspace.worktree.Branch, boundSHA))
}

// journalRunBranch records the run's own branch reference from the workspace
// that provisioned it, stamped with the commit that branch is at.
func (r *Runner) journalRunBranch(ctx context.Context, jr journalAppender, in StartInput, wt *worktree.Worktree) error {
	branchSHA, err := wt.HeadSHA(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("resolve workspace branch %q: %w", wt.Branch, err)
	}
	in.WorkspaceBranch = wt.Branch
	in.WorkspaceBranchSHA = branchSHA
	if err := r.recordRunBranch(jr, in); err != nil {
		return fmt.Errorf("runner: journal run branch for %q: %w", in.RunID, err)
	}
	return nil
}

// journalReboundBranch records checkedOut when it is the run-scoped rebinding
// this stage was provisioned against and no earlier stage recorded it already.
// A stage on the run's own nominal branch records nothing here: that branch is
// already in the journal as ref.touched{kind:branch}.
func (r *Runner) journalReboundBranch(jr journalAppender, in StartInput, tf taskFrame, checkedOut, boundSHA string) error {
	if tf.workspaceBranch == "" || tf.reboundRecorded == nil ||
		checkedOut != tf.workspaceBranch || *tf.reboundRecorded == checkedOut {
		return nil
	}
	if err := r.recordReboundWorkspaceBranch(jr, in, checkedOut, boundSHA); err != nil {
		return fmt.Errorf("runner: journal rebound workspace branch for %q: %w", in.RunID, err)
	}
	*tf.reboundRecorded = checkedOut
	return nil
}

func deferRunBranchProvenance(kind journal.TriggerKind) bool {
	return kind == journal.TriggerSchedule || kind == journal.TriggerItem
}

func hasRunBranchRef(events []journal.Event) bool {
	for _, event := range events {
		if event.Type == journal.EventRefTouched && event.ExternalRef != nil && event.ExternalRef.Kind == "branch" {
			return true
		}
	}
	return false
}

// rebindWorkspaceBranch reports the branch a finished task's result rebinds the
// run's workspace to, or "" if it rebinds nothing. Fails closed on every
// doubtful case: a non-deterministic producer, a non-string value, or a branch
// outside the run-branch namespace (nsPrefix, the run's gaggle-resolved branch
// namespace root) are all ignored rather than honored.
func rebindWorkspaceBranch(t apiv1.Task, result apiv1.ResultEnvelope, nsPrefix string) string {
	if t.Type != apiv1.TaskDeterministic {
		return ""
	}
	return workspaceBranchFrom(result.Outputs, nsPrefix)
}

// workspaceBranchFrom reads WorkspaceBranchOutput out of a stage's scalar
// Outputs. Shared with resume.go, whose journaled outputs are the same values
// under map[string]any rather than map[string]interface{} (identical types,
// different spelling). nsPrefix is the run's gaggle-resolved run-branch
// namespace root (Runner.branchNamespaceFor); a value outside it is rejected.
//
// Deliberately does NOT stringify non-strings the way internal/gate's
// stringField does: a branch name is not a value to coerce, and
// `"workspaceBranch": false` becoming a branch literally named "false" is a
// worse outcome than ignoring a malformed emission. Values outside the
// run-branch namespace are ignored for the same reason — "main" is the
// dangerous case, and nothing legitimate needs it.
func workspaceBranchFrom(outputs map[string]interface{}, nsPrefix string) string {
	v, ok := outputs[WorkspaceBranchOutput]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, nsPrefix) {
		return ""
	}
	return s
}
