package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/executor"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

// remediationTarget is the pull request a pr-remediation run was DISPATCHED
// for (#3985). `goobers run --pr <n>` is delivered as a synthetic
// pull_request webhook, so the operator's argument survives only in the run's
// trigger reference; merge-review's pr-select has consumed it since #1318,
// while the remediation lane went straight to its own priority ranking and
// silently remediated whichever PR its policy preferred.
//
// Every remediation selector therefore resolves this target and narrows its
// final candidate set through it. The narrowing is FAIL-CLOSED in both
// directions: a targeted run can only ever act on the PR the trigger named,
// and a target that is not selectable ends the run rather than falling back
// to the lane's next-best candidate. An untargeted (scheduled) run is
// unaffected — remediationTarget.apply returns the candidate set unchanged —
// which is what keeps the `37 * * * *` tick's behavior byte-identical.
type remediationTarget struct {
	number   int
	targeted bool
}

// remediationTargetFromEnv reads the target from the stage environment the
// executor populates for every stage of a run (executor.TriggerRefEnvVar).
func remediationTargetFromEnv() remediationTarget {
	return remediationTargetFromTriggerRef(os.Getenv(executor.TriggerRefEnvVar))
}

// remediationTargetFromTriggerRef resolves a trigger reference to its target.
// Anything that is not a pull_request delivery carrying a positive PR number
// — a schedule ref, a bare signal name, a malformed number — resolves to the
// untargeted zero value, so a selector can never mistake an unparseable ref
// for "target PR 0".
func remediationTargetFromTriggerRef(ref string) remediationTarget {
	pullID, targeted := webhookhttp.PullNumberFromTriggerRef(ref)
	if !targeted {
		return remediationTarget{}
	}
	number, err := strconv.Atoi(pullID)
	if err != nil || number <= 0 {
		return remediationTarget{}
	}
	return remediationTarget{number: number, targeted: true}
}

// remediationTargetStage is one narrowing step of a remediation lane's
// selection pipeline, paired with the operator-facing reason a targeted PR
// that is absent from it was refused. Stages are supplied in pipeline order
// so the refusal names the FIRST filter that dropped the target, which is the
// one an operator can act on.
type remediationTargetStage struct {
	prs    []providers.PullRequestSummary
	reason string
}

const (
	// remediationTargetFilteredReason covers filterRemediationPullRequests'
	// exclusions (and the ADO label-tier equivalent).
	remediationTargetFilteredReason = "is excluded from remediation (goobers:needs-human, an escalation that still blocks, blocked on an open sibling PR, or its head branch is held by another run's worktree)"
	// remediationTargetClaimedReason covers the shared PR lease namespace:
	// merge-review or another remediation run already holds this PR.
	remediationTargetClaimedReason = "is already claimed by another run in the shared pull-request lease namespace"
	// remediationTargetIneligibleReason covers selectRemediationCandidates:
	// the PR is visible and unclaimed but carries no remediation signal.
	remediationTargetIneligibleReason = "does not need remediation this cycle (no " + needsRemediationLabel + " label, CI is not failing, and it is not a crowned behind-base lander)"
)

// remediationTargetUnlistedReason names the lane's own selection scope, so an
// operator who targeted a PR on another base — or outside the goober branch
// namespace — is told exactly which scope excluded it.
func remediationTargetUnlistedReason(base, headPrefix string) string {
	return fmt.Sprintf("is not an open pull request in this lane's selection scope (base %q, head prefix %q)", base, headPrefix)
}

// apply narrows a remediation lane's selection to the dispatched target. The
// LAST stage carries the lane's final candidate set; earlier stages exist only
// to attribute a refusal to the filter that actually dropped the target.
//
// An untargeted run gets the final candidate set back unchanged and an empty
// refusal — scheduled selection is untouched. A targeted run gets either
// exactly the target (never more, never a substitute) or an empty set plus the
// refusal reason its caller reports as an explicit no-work business outcome.
func (t remediationTarget) apply(stages ...remediationTargetStage) ([]providers.PullRequestSummary, string) {
	var candidates []providers.PullRequestSummary
	if len(stages) > 0 {
		candidates = stages[len(stages)-1].prs
	}
	if !t.targeted {
		return candidates, ""
	}
	for _, stage := range stages {
		if remediationTargetIndex(stage.prs, t.number) < 0 {
			return nil, fmt.Sprintf("targeted PR #%d %s", t.number, stage.reason)
		}
	}
	// Unreachable when the final stage contains the target, which the loop
	// above just established; kept so a future caller that supplies no stages
	// still fails closed rather than selecting by policy.
	index := remediationTargetIndex(candidates, t.number)
	if index < 0 {
		return nil, fmt.Sprintf("targeted PR #%d is not selectable by this remediation lane", t.number)
	}
	return []providers.PullRequestSummary{candidates[index]}, ""
}

func remediationTargetIndex(prs []providers.PullRequestSummary, number int) int {
	for i, pr := range prs {
		if pr.Number == number {
			return i
		}
	}
	return -1
}
