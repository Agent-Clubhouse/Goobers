package runner

import (
	"context"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/baseline"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// SharedBaselineFailureCode is the stage-failure code a local-ci failure
// carries once it is attributed to the target branch rather than to the run's
// own diff (#2971). It is a recognized non-retryable disposition
// (escalateErrorCodes), so the affected run parks against the shared blocker
// through the existing #415 route instead of spending an implementation repass
// re-deriving that there is nothing branch-local to fix.
const SharedBaselineFailureCode = "SHARED_BASELINE_FAILURE"

// baselineClassificationKind is the runner.annotation kind that records a
// baseline comparison. Every classification is journaled — including
// pr-introduced and unknown — so an operator can tell "compared and blamed the
// branch" from "never compared".
const baselineClassificationKind = "baseline.classification"

// BaselineHealth compares a failing CI command against the target branch's own
// health at the pinned base SHA. internal/baseline provides the production
// implementation; nil (the default) leaves every failure attributed exactly as
// it was before this seam existed.
type BaselineHealth interface {
	// BaseSHA resolves the repository's current target-branch commit — the
	// base a syncBase stage merged before it ran. repoURL is the runner's own
	// resolved clone URL, so the implementation never re-derives it.
	BaseSHA(ctx context.Context, repo apiv1.RepoRef, repoURL string) (string, error)
	// Classify attributes one failing CI observation.
	Classify(ctx context.Context, req baseline.Request) (baseline.Decision, error)
	// ReleaseReady un-parks the subjects whose shared baseline failure is no
	// longer current — the base advanced, or the baseline recovered — and
	// reports which ones it released.
	ReleaseReady(ctx context.Context, repo apiv1.RepoRef, repoURL string) ([]baseline.Waiter, error)
}

// baselineReleaseKind is the runner.annotation kind recording that a run start
// released parked subjects.
const baselineReleaseKind = "baseline.release"

// releaseBaselineParks un-parks subjects waiting on a shared baseline failure
// whose base has since advanced (or gone green), at the start of every run that
// touches the repository. A parked subject is not selectable, so it can never
// re-measure the base itself — some OTHER run has to notice the base moved, and
// every run of any workflow on that repository is exactly the cheap, always-
// available trigger for that. Best effort throughout: a repository this run
// cannot resolve, an unreadable store, or a failed journal append leaves the
// parks exactly as they were rather than disturbing the run.
func (r *Runner) releaseBaselineParks(ctx context.Context, ws *walkState) {
	health := r.cfg.BaselineHealth
	if health == nil || !machineUsesRepo(ws.in.Machine) {
		return
	}
	repoURL, err := r.cfg.RepoCloneURL(ws.in.RepoRef)
	if err != nil {
		return
	}
	released, err := health.ReleaseReady(ctx, ws.in.RepoRef, repoURL)
	if err != nil || len(released) == 0 {
		return
	}
	subjects := make([]string, 0, len(released))
	for _, waiter := range released {
		subjects = append(subjects, waiter.Subject)
	}
	_ = ws.jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"kind":     baselineReleaseKind,
			"subjects": strings.Join(subjects, ","),
			"released": len(subjects),
		},
	})
}

// ciCommand returns the configured local-ci stage's full argv, or nil when the
// machine declares no such stage or it carries no command.
func ciCommand(machine *workflow.Machine) []string {
	for _, task := range machine.Def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Name != localCIStageName {
			continue
		}
		if task.Run == nil || len(task.Run.Command) == 0 {
			return nil
		}
		return append([]string(nil), task.Run.Command...)
	}
	return nil
}

// baselineCandidate reports whether a finished stage is one whose failure is
// worth comparing against the base: the well-known local-ci stage failing with
// the generic nonzero_exit code. A stage that failed for a structurally typed
// reason (a sync conflict, a missing result file) already knows its own cause
// and is never re-attributed here.
func baselineCandidate(task apiv1.Task, result apiv1.ResultEnvelope) bool {
	return task.Name == localCIStageName &&
		task.Type == apiv1.TaskDeterministic &&
		result.Status == apiv1.ResultFailure &&
		result.Error != nil &&
		result.Error.Code == "nonzero_exit"
}

// baselineFailureText is the bounded evidence a signature is derived from: the
// stage's summary plus its error message, which together carry the executor's
// extracted failure diagnostic (internal/executor summarizeCommandFailure).
// baseline.FailureSignatureText reduces this and the probe's raw transcript to
// the same diagnostic, so the two halves of a comparison are comparable.
func baselineFailureText(result apiv1.ResultEnvelope) string {
	parts := make([]string, 0, 2)
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		parts = append(parts, summary)
	}
	if result.Error != nil {
		if message := strings.TrimSpace(result.Error.Message); message != "" {
			parts = append(parts, message)
		}
	}
	return strings.Join(parts, "\n")
}

// applyBaselineDecision rewrites a failing local-ci result into the parked
// shared-baseline disposition. A decision that does not park (the branch
// introduced the failure, the comparison was not possible, or the shared repair
// lane is explicitly enabled) leaves the result untouched.
func applyBaselineDecision(result apiv1.ResultEnvelope, decision baseline.Decision) (apiv1.ResultEnvelope, bool) {
	if decision.Class != baseline.ClassSharedBaselineFailure || !decision.Park {
		return result, false
	}
	result.Summary = fmt.Sprintf("shared baseline failure: %s", decision.Reason)
	result.Error = &apiv1.ErrorInfo{
		Code:      SharedBaselineFailureCode,
		Message:   decision.Reason,
		Retryable: false,
	}
	return result, true
}

// baselineAnnotation builds the journal event recording one classification.
func baselineAnnotation(stage string, decision baseline.Decision, parked bool) journal.Event {
	return journal.Event{
		Type:  journal.EventRunnerAnnotation,
		Stage: stage,
		Runner: map[string]any{
			"kind":        baselineClassificationKind,
			"class":       string(decision.Class),
			"baseSha":     decision.BaseSHA,
			"fingerprint": decision.Fingerprint,
			"blocker":     decision.BlockerKey,
			"waiting":     decision.Waiting,
			"parked":      parked,
			"reason":      decision.Reason,
		},
	}
}

// classifyBaselineFailure attributes a failing local-ci stage to the branch or
// to its base and, for a shared baseline failure, parks this run against the
// durable shared blocker. The classification is advisory: a resolver or probe
// error is journaled and the original result is returned unchanged, because a
// baseline that cannot be measured must never change how a run is routed.
func (r *Runner) classifyBaselineFailure(ctx context.Context, ws *walkState, task apiv1.Task, result apiv1.ResultEnvelope) (apiv1.ResultEnvelope, error) {
	health := r.cfg.BaselineHealth
	if health == nil || !baselineCandidate(task, result) {
		return result, nil
	}
	command := ciCommand(ws.in.Machine)
	if len(command) == 0 {
		return result, nil
	}
	repo := ws.in.RepoRef
	repoURL, err := r.cfg.RepoCloneURL(repo)
	if err != nil {
		return result, r.annotateBaselineError(ws, task.Name, err)
	}
	baseSHA, err := health.BaseSHA(ctx, repo, repoURL)
	if err != nil {
		return result, r.annotateBaselineError(ws, task.Name, err)
	}
	req := baseline.Request{
		Repo:        baseline.RepoKey(repo.Owner, repo.Name),
		RepoURL:     repoURL,
		BaseSHA:     baseSHA,
		Command:     command,
		FailureText: baselineFailureText(result),
		RunID:       ws.in.RunID,
	}
	if ws.in.Item != nil {
		req.Waiter = ws.in.Item.ID
	}
	decision, err := health.Classify(ctx, req)
	if err != nil {
		return result, r.annotateBaselineError(ws, task.Name, err)
	}
	classified, parked := applyBaselineDecision(result, decision)
	if aerr := ws.jr.Append(baselineAnnotation(task.Name, decision, parked)); aerr != nil {
		return result, fmt.Errorf("runner: journal baseline classification for %q: %w", task.Name, aerr)
	}
	return classified, nil
}

func (r *Runner) annotateBaselineError(ws *walkState, stage string, cause error) error {
	if aerr := ws.jr.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		Stage: stage,
		Runner: map[string]any{
			"kind":  baselineClassificationKind,
			"class": string(baseline.ClassUnknown),
			"error": cause.Error(),
		},
	}); aerr != nil {
		return fmt.Errorf("runner: journal baseline classification error for %q: %w", stage, aerr)
	}
	return nil
}
