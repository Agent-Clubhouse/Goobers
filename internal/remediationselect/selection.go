package remediationselect

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// PrReferencePattern matches a "PR #N" reference.
var PrReferencePattern = regexp.MustCompile(`(?i)\bPR\s*#\s*([0-9]+)\b`)

// HashReferencePattern additionally accepts the bare "#N" form ("with #597's
// runs list --json path") — live merge-review verdicts reference the selected
// PR this way inside finding messages, without the "PR" prefix
// PrReferencePattern requires.
var HashReferencePattern = regexp.MustCompile(`#\s*([0-9]+)\b`)

// RemediationPriority ranks the signal that makes a PR a remediation candidate.
type RemediationPriority uint8

// The remediation priorities, weakest first.
const (
	RemediationPriorityNone RemediationPriority = iota
	RemediationPriorityBehindBase
	RemediationPriorityFailingCI
	RemediationPriorityNeedsRemediation
)

// FilterRemediationPullRequests applies the shared exclusion rules before
// either the API fast lane or the full worktree-backed remediation path selects
// a candidate. heldBranches is nil for the API-only lane, which can safely
// update a branch even while another local worktree has it checked out. The
// returned counts identify eligible crowned landers from their live parked
// dependents without a second provider scan.
func FilterRemediationPullRequests(ctx context.Context, provider Provider, repo providers.RepositoryRef, prs []providers.PullRequestSummary, heldBranches map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
	var eligible []providers.PullRequestSummary
	blockedDependents := make(map[int]int)
	for _, pr := range prs {
		if heldBranches[pr.Head] {
			continue
		}
		if HasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
			continue
		}
		blocked, err := EscalationStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return nil, nil, fmt.Errorf("check escalation state for PR #%d: %w", pr.Number, err)
		}
		if blocked {
			continue
		}
		blockedState, err := LiveBlockedOnSiblingState(ctx, provider, repo, pr)
		if err != nil {
			return nil, nil, fmt.Errorf("check blocked-on-sibling state for PR #%d: %w", pr.Number, err)
		}
		liveBlockers := blockedState.Blockers
		for _, blocker := range liveBlockers {
			blockedDependents[blocker]++
		}
		if ShouldParkRemediation(pr, blockedState) {
			continue
		}
		eligible = append(eligible, pr)
	}
	return eligible, blockedDependents, nil
}

// ShouldParkRemediation reports whether a PR parked behind live siblings is held out of remediation.
func ShouldParkRemediation(pr providers.PullRequestSummary, blockedState BlockedOnSiblingState) bool {
	if len(blockedState.Blockers) == 0 {
		return false
	}
	if pr.CheckState == providers.CheckStateFailing {
		return false
	}
	if strings.HasPrefix(blockedState.Reason, "foundation-coupled to PR #") {
		return true
	}
	return !HasAnyLabel(pr.Labels, []string{NeedsRemediationLabel})
}

// RemediationPriorityFor classifies a single PR's remediation signal,
// independent of its peers — a needs-remediation label or failing CI is a
// strong signal (the two share one oldest-first order: see
// StrongRemediationCandidates), and neither implies anything about whether the
// PR is merely behind its base (that's a fallback tier, checked only when
// nothing clears these two: see selectRemediationCandidates). Escalation exclusion is NOT this function's
// concern: runGatherPRContext's self-heal-aware escalationStillBlocks
// (#716) pre-filters prs before selectRemediationCandidates ever sees them,
// so every pr reaching here has already cleared that check — a static
// re-check of the label here would incorrectly re-exclude a PR that just
// self-healed (still labeled merge-escalated, but its head/base moved past
// the recorded escalation snapshot).
func RemediationPriorityFor(pr providers.PullRequestSummary) RemediationPriority {
	switch {
	case HasAnyLabel(pr.Labels, []string{NeedsRemediationLabel}):
		return RemediationPriorityNeedsRemediation
	case pr.CheckState == providers.CheckStateFailing:
		return RemediationPriorityFailingCI
	}
	return RemediationPriorityNone
}

// SelectRemediationCandidates returns every open PR carrying a strong
// remediation signal (needs-remediation or failing CI), oldest PR first — see
// StrongRemediationCandidates for why the two strong signals share one FIFO
// order rather than ranking as separate tiers. Offering every strong candidate
// to the claim ledger lets concurrent runs fall through when earlier
// candidates are already claimed instead of leaving eligible work idle.
//
// Only when no PR clears either strong tier does a lander merely behind its
// base become eligible, and then only if it is either crowned or alone. A crown
// is materialized by at least one live parked dependent naming the PR as a
// blocker. This keeps the rest of an overlapping wave parked until its
// predecessor lands instead of eagerly rebasing every behind-base sibling after
// each merge. Checking "behind base" requires fetching candidate branches, so
// behindBase is invoked only for those two cases when nothing stronger exists.
//
// #4163: the crown requirement alone made this path unreachable on the shipped
// default. The quota argument behind it is entirely about sibling ordering — an
// uncrowned PR in a wave will go behind again as soon as its predecessor lands,
// so updating it now buys nothing — and that argument says nothing at all about
// a lane holding one PR. `implementation` ships maxConcurrentRuns: 1, so such a
// lane never accumulates dependents, blockedDependents is 0 for the only PR
// there is on every tick, and behind-base could not fire even once. A solitary
// candidate has no predecessor to wait for: updating it once is the whole cost,
// not the first installment of many.
func SelectRemediationCandidates(prs []providers.PullRequestSummary, blockedDependents map[int]int, behindBase func(providers.PullRequestSummary) (bool, error)) ([]providers.PullRequestSummary, RemediationPriority, error) {
	candidates, best := StrongRemediationCandidates(prs)
	if len(candidates) > 0 {
		return candidates, best, nil
	}

	candidates, err := BehindBaseRemediationCandidates(prs, blockedDependents, behindBase, len(prs) == 1)
	if err != nil {
		return nil, RemediationPriorityNone, err
	}
	if len(candidates) > 0 {
		best = RemediationPriorityBehindBase
	}
	return candidates, best, nil
}

// BehindBaseRemediationCandidates is the fallback tier: every crowned lander
// (or the lane's solitary PR) in prs that is behind its base, oldest first.
func BehindBaseRemediationCandidates(prs []providers.PullRequestSummary, blockedDependents map[int]int, behindBase func(providers.PullRequestSummary) (bool, error), solitary bool) ([]providers.PullRequestSummary, error) {
	var candidates []providers.PullRequestSummary
	for _, pr := range prs {
		// Same rationale as remediationPriorityFor: escalation exclusion
		// already happened upstream (self-heal-aware), so no re-check here.
		if blockedDependents[pr.Number] == 0 && !solitary {
			continue
		}
		behind, err := behindBase(pr)
		if err != nil {
			return nil, err
		}
		if behind {
			candidates = append(candidates, pr)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Number < candidates[j].Number
	})
	return candidates, nil
}

// StrongRemediationCandidates returns the PRs with a needs-remediation label or failing CI, oldest first, and the strongest signal among them.
func StrongRemediationCandidates(prs []providers.PullRequestSummary) ([]providers.PullRequestSummary, RemediationPriority) {
	var candidates []providers.PullRequestSummary
	best := RemediationPriorityNone
	for _, pr := range prs {
		priority := RemediationPriorityFor(pr)
		if priority == RemediationPriorityNone {
			continue
		}
		candidates = append(candidates, pr)
		best = max(best, priority)
	}
	// #6482: needs-remediation and failing CI share ONE oldest-first (lowest
	// PR number) order. They used to be two strict tiers — every
	// needs-remediation PR ahead of every failing-CI PR — so any steady supply
	// of needs-remediation PRs (pr-comment-watch adds them on its own) pushed a
	// PR whose build went red behind it indefinitely: each cycle remediated the
	// head of the needs-remediation tier and the failing-CI tier never reached
	// the front. Under one FIFO order a PR waits only behind OLDER open PRs'
	// remediation turns — ordered by PR age, not by how long the signal has
	// been present. PR numbers only grow, so a stream of newly opened PRs can
	// never overtake it. An older PR can re-enter ahead of it when it picks
	// up a signal again (a new comment relabelled by pr-comment-watch, a
	// merge-queue eviction, a merge-review demotion, post-merge triage), but
	// there are finitely many older open PRs and each re-entry needs such an
	// outside event, so the wait stays bounded in practice — deterministic,
	// with no clock or durable state. Both signals already
	// block merge equally (merge-review refuses either), so neither has a
	// claim to starve the other. The behind-base fallback is still a separate,
	// weaker tier: selectRemediationCandidates reaches it only when no strong
	// candidate exists.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Number < candidates[j].Number
	})
	return candidates, best
}

// VerdictHasSubstantiveFindingForPR reports whether verdict carries a substantive finding attributable to PR number.
func VerdictHasSubstantiveFindingForPR(verdict *apiv1.Verdict, prNumber int, minSeverity apiv1.Severity) bool {
	if verdict == nil {
		return false
	}
	// A standing finding (#5890) always counts, whatever its class or
	// severity: merge-pr blocks on it at the info floor, so every healer must
	// route it to remediation rather than heal past it.
	if len(verdict.StandingFindings) > 0 {
		return true
	}
	target := strconv.Itoa(prNumber)
	for _, finding := range verdict.Findings {
		if SubstantiveFindingAppliesToPR(finding, target, minSeverity) {
			return true
		}
	}
	return false
}

// SubstantiveFindingAppliesToPR reports whether one finding is attributable to PR number.
func SubstantiveFindingAppliesToPR(finding apiv1.Finding, target string, minSeverity apiv1.Severity) bool {
	// A finding's class routes it to the right remediation action, so an
	// explicitly non-code-change class (cross-pr-blocked, rebase-needed) is not
	// a substantive-rework signal. But an UNSET class is treated liberally — the
	// same liberal default the unset-Severity branch below uses — because a
	// reviewer that omits the tag must not silently drop a real defect from
	// remediation. (A live ADO command-injection finding carried no class and
	// stalled the whole loop until this backstop existed; the severity floor
	// still filters low-value classless noise.)
	if finding.Class != "" && !finding.Class.RequiresCodeChange() {
		return false
	}
	// An unset Severity (verdicts recorded before this field existed, or
	// any evaluator that never populates it) always counts — the liberal
	// default must reproduce today's behavior exactly.
	if finding.Severity != "" && finding.Severity.Rank() < minSeverity.Rank() {
		return false
	}
	locationRefs := PrReferencePattern.FindAllStringSubmatch(finding.Location, -1)
	if len(locationRefs) == 0 || ReferencesTarget(locationRefs, target) {
		return true
	}
	return ReferencesTarget(HashReferencePattern.FindAllStringSubmatch(finding.Message, -1), target)
}

// ReferencesTarget reports whether any captured PR-number reference equals
// target (matches come from prReferencePattern or hashReferencePattern, both
// of which capture the number as the first submatch).
func ReferencesTarget(matches [][]string, target string) bool {
	for _, match := range matches {
		if match[1] == target {
			return true
		}
	}
	return false
}
