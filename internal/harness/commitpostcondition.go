package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/journal"
)

// ErrorCodeUncommittedChanges is the typed failure code for a writable
// agentic stage that reported success while leaving its work uncommitted
// (#5182). internal/runner.UncommittedChangesCode carries the same string;
// the runner treats it as a retryable policy failure, so the stage's own
// retry budget sends the work back instead of the reviewer gate failing an
// empty committed diff terminally.
const ErrorCodeUncommittedChanges = "UNCOMMITTED_CHANGES"

// modifyRepositoryPolicyAction is the declared policy action whose contract is
// "make and commit repository changes" (docs/reference/workflow-primitives/
// policy-actions.md). It is the structural signal that a stage's success has a
// commit postcondition — keyed on the DSL declaration, never on a stage name.
const modifyRepositoryPolicyAction = "modify-repository"

// uncommittedDiffArtifactSuffix names the recovery artifact for the work an
// uncommitted stage left in its workspace.
const uncommittedDiffArtifactSuffix = "/uncommitted-diff.patch"

// commitPostconditionTimeout bounds each git inspection. The checks read local
// state only, so a healthy run finishes in milliseconds.
const commitPostconditionTimeout = time.Minute

// ErrUncommittedChanges marks a success completion whose workspace still holds
// uncommitted changes and no new commit. Adapters treat it as a repairable
// completion error, so the same session is told to commit its own work; the
// Executor never commits on the agent's behalf.
var ErrUncommittedChanges = errors.New(ErrorCodeUncommittedChanges + ": the stage reported success but left its changes uncommitted and created no commit")

// commitPostcondition is the HEAD a writable stage started from. A success
// that leaves HEAD there while the working tree is dirty did the work and
// skipped the commit.
type commitPostcondition struct {
	workspace string
	head      string
}

// uncommittedWork is what the Executor found when the postcondition failed.
type uncommittedWork struct {
	status string
	diff   *apiv1.ArtifactPointer
}

// observeCommitPostcondition snapshots the starting HEAD for a stage whose
// success must include a commit. It returns nil — no check — for a review, a
// stage that does not declare modify-repository, or a workspace that is not a
// git checkout on a branch (scratch and detached read-only checkouts).
func observeCommitPostcondition(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope) *commitPostcondition {
	if mode != ModeInvoke || env.Workspace == "" || !slices.Contains(env.PolicyActions, modifyRepositoryPolicyAction) {
		return nil
	}
	if _, err := postconditionGit(ctx, env.Workspace, nil, "symbolic-ref", "-q", "HEAD"); err != nil {
		return nil
	}
	head, err := postconditionGit(ctx, env.Workspace, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil
	}
	return &commitPostcondition{workspace: env.Workspace, head: strings.TrimSpace(head)}
}

// armCommitPostcondition observes the starting HEAD for a stage whose success
// must include a commit (#5182) and, when there is one, wraps req's completion
// validator so an adapter's bounded repair turn tells the same session to
// commit. Returns nil — every method is then a no-op — when no check applies.
func armCommitPostcondition(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope, req *RunRequest) *commitPostcondition {
	c := observeCommitPostcondition(ctx, mode, env)
	if c != nil {
		req.ValidateCompletion = c.wrapValidator(ctx, req.ValidateCompletion)
	}
	return c
}

// settle passes an adapter's outcome through, except that an adapter error
// that is only the commit postcondition — the repair turn did not commit
// either — is cleared: the completion itself is valid, so inspect judges it as
// an UNCOMMITTED_CHANGES failure result rather than an adapter fault.
//
// Only the postcondition member is removed from a joined error (codex joins
// receipt and telemetry failures onto its completion error after returning),
// so every other failure still surfaces.
func (c *commitPostcondition) settle(out Outcome, err error) (Outcome, error) {
	if c == nil {
		return out, err
	}
	return out, withoutUncommittedChanges(err)
}

func withoutUncommittedChanges(err error) error {
	if err == nil || !errors.Is(err, ErrUncommittedChanges) {
		return err
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil
	}
	var rest []error
	for _, member := range joined.Unwrap() {
		if kept := withoutUncommittedChanges(member); kept != nil {
			rest = append(rest, kept)
		}
	}
	return errors.Join(rest...)
}

// repairExit decides what a repair turn that could not run, or that failed,
// leaves behind. For a completion that failed only the commit postcondition
// the original, schema-valid completion stands — the Executor then reports
// UNCOMMITTED_CHANGES and records the diff — rather than turning finished
// work into a timeout or an adapter fault. Any other repairable error keeps
// the adapter's existing behavior: the repair failure replaces it. Returns
// (runErr, completionErr).
func repairExit(completionErr, failure error) (error, error) {
	if errors.Is(completionErr, ErrUncommittedChanges) {
		return nil, completionErr
	}
	return failure, nil
}

// wrapValidator adds the commit postcondition to a completion validator, so an
// adapter's bounded repair turn sends an uncommitted success back to the same
// session. Inspection errors leave the completion valid: this check only ever
// narrows success, and the reviewer gate's empty-diff guard still fails closed
// behind it.
func (c *commitPostcondition) wrapValidator(ctx context.Context, base func([]byte) error) func([]byte) error {
	return func(payload []byte) error {
		if base != nil {
			if err := base(payload); err != nil {
				return err
			}
		}
		if status, dirty := c.violatedBy(ctx, payload); dirty {
			return fmt.Errorf("%w; uncommitted paths:\n%s", ErrUncommittedChanges, status)
		}
		return nil
	}
}

// violatedBy reports whether payload claims success while HEAD has not moved
// and the working tree holds changes, returning the porcelain status on a
// violation.
func (c *commitPostcondition) violatedBy(ctx context.Context, payload []byte) (string, bool) {
	var claimed struct {
		Status apiv1.ResultStatus `json:"status"`
	}
	if json.Unmarshal(payload, &claimed) != nil || claimed.Status != apiv1.ResultSuccess {
		return "", false
	}
	head, err := postconditionGit(ctx, c.workspace, nil, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != c.head {
		return "", false
	}
	args := append([]string{"status", "--porcelain=v1", "--untracked-files=normal", "--"}, harnessOwnedPathspecs()...)
	status, err := postconditionGit(ctx, c.workspace, nil, args...)
	if err != nil || strings.TrimSpace(status) == "" {
		return "", false
	}
	return strings.TrimRight(status, "\n"), true
}

// captureDiff renders the uncommitted work, untracked files included, as a
// binary patch against HEAD. It stages into a throwaway index so the agent's
// real index is never touched.
func (c *commitPostcondition) captureDiff(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "goobers-uncommitted-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := postconditionGit(ctx, c.workspace, env, "read-tree", "HEAD"); err != nil {
		return nil, err
	}
	addArgs := append([]string{"add", "-A", "--"}, harnessOwnedPathspecs()...)
	if _, err := postconditionGit(ctx, c.workspace, env, addArgs...); err != nil {
		return nil, err
	}
	diff, err := postconditionGit(ctx, c.workspace, env, "diff", "--cached", "--binary", "--no-color", "--no-ext-diff", "HEAD")
	if err != nil {
		return nil, err
	}
	return []byte(diff), nil
}

// harnessOwnedPathspecs limits the inspection to the agent's work: the
// harness's own scratch (completion file, telemetry, context) and materialized
// goober assets are never part of the stage's deliverable, even in a workspace
// whose git excludes were not provisioned to hide them.
func harnessOwnedPathspecs() []string {
	return []string{".", ":(exclude,top).goobers", ":(exclude,top)" + gooberassets.WorkspaceDir}
}

// inspect re-checks the final completion and, on a violation, records the
// uncommitted work as a recovery artifact. Best-effort recording: the failure
// it reports does not depend on the artifact.
func (c *commitPostcondition) inspect(ctx context.Context, e *Executor, stage string, payload []byte) *uncommittedWork {
	if c == nil {
		return nil
	}
	status, dirty := c.violatedBy(ctx, payload)
	if !dirty {
		return nil
	}
	work := &uncommittedWork{status: status}
	diff, err := c.captureDiff(ctx)
	if err != nil || len(diff) == 0 {
		return work
	}
	ref, err := e.artifacts.RecordArtifact(stage+uncommittedDiffArtifactSuffix, e.scrubber.Scrub(diff))
	if err != nil {
		return work
	}
	ptr := refToPointer(ref, "text/x-diff")
	work.diff = &ptr
	return work
}

// uncommittedChangesResult turns a success that failed the commit
// postcondition into a retryable UNCOMMITTED_CHANGES failure.
func uncommittedChangesResult(result apiv1.ResultEnvelope, work *uncommittedWork) apiv1.ResultEnvelope {
	message := ErrUncommittedChanges.Error() + "; commit the work to the run branch before reporting success"
	if work.status != "" {
		message += "\nuncommitted paths:\n" + work.status
	}
	if work.diff != nil {
		message += "\nthe uncommitted diff is preserved as " + work.diff.Path
	}
	result.Status = apiv1.ResultFailure
	result.Summary = "stage reported success but left its changes uncommitted"
	result.Error = journal.ErrorInfoFor(ErrorCodeUncommittedChanges, errors.New(message), true)
	return result
}

func postconditionGit(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitPostconditionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
