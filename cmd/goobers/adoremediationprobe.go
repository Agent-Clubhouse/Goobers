package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// adoRemediationCIProbeLimit bounds how many pull requests one scheduled Azure
// DevOps remediation cycle reads CI state for (#6482). A probe is the PR
// detail read that names its project, then its policy evaluations, plus one
// build read per rejected build policy once cancelled-build classification
// applies (#6817). The lane runs a few cycles an hour, so the bound only caps
// a repository with an unusually large open-PR set. A larger set is covered
// over successive cycles by the persisted window cursor
// (advanceADOCIProbeWindow), never by permanently probing the same oldest PRs.
const adoRemediationCIProbeLimit = 25

// adoCIProbePlan is how one gather-pr-context cycle chooses which ADO pull
// requests to read CI state for.
type adoCIProbePlan struct {
	// target is the run's trigger target. A targeted run (`--pr N`) reads the
	// target PR alone, outside the bound: every other PR would be discarded by
	// the target filter anyway, so reading it costs requests and lets an
	// unrelated PR's read error fail the operator's run.
	target remediationTarget
	// available drops PRs another run holds a claim on; reading their CI
	// cannot change what this run selects.
	available func([]providers.PullRequestSummary) ([]providers.PullRequestSummary, error)
	// window picks the bounded, rotating slice of probeable PR numbers.
	window func(probeable []int) ([]int, error)
	// probe reads one PR's CI state and marks it failing when it is.
	probe func(context.Context, *providers.PullRequestSummary) error
}

// adoRemediationCandidates is the Azure DevOps eligibility pass (#6482): drop
// PRs another local worktree holds or a human was asked to take
// (goobers:needs-human), then discover failing CI.
//
// ListPullRequests on ADO carries no CI state, so before this the lane saw
// only its label tier: a PR whose build-validation policy a human's push
// turned red was invisible to scheduled remediation until merge-review
// demoted it to goobers:needs-remediation. The probe reads one PR's policy
// evaluations — the same CI classification gather-ci-failures reports
// (resolveADOSelectedCheckState, ADO-N22) — and marks the PR failing, which
// puts it in the strong tier (strongRemediationCandidates) beside the labelled
// PRs, in the same oldest-first order.
//
// Only a PR whose tier the read can change is probed (adoCIProbeCanChangeTier),
// and adoCIProbeNumbers bounds and rotates which of those this cycle reads. A
// probe error fails the stage, as a check-state read does on GitHub
// (resolveRemediationCheckStates): an unreadable CI state is not "passing".
func adoRemediationCandidates(
	ctx context.Context,
	prs []providers.PullRequestSummary,
	held map[string]bool,
	plan adoCIProbePlan,
) ([]providers.PullRequestSummary, error) {
	eligible := make([]providers.PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		if !held[pr.Head] && !hasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
			eligible = append(eligible, pr)
		}
	}
	numbers, err := adoCIProbeNumbers(eligible, plan)
	if err != nil {
		return nil, err
	}
	probe := make(map[int]bool, len(numbers))
	for _, number := range numbers {
		probe[number] = true
	}
	for i := range eligible {
		if !probe[eligible[i].Number] {
			continue
		}
		if err := plan.probe(ctx, &eligible[i]); err != nil {
			return nil, fmt.Errorf("resolve CI state for PR #%d: %w", eligible[i].Number, err)
		}
	}
	return eligible, nil
}

// adoCIProbeNumbers chooses the PRs this cycle reads CI state for: the target
// alone on a targeted run, else the next bounded window of the probeable PRs
// no other run holds.
func adoCIProbeNumbers(eligible []providers.PullRequestSummary, plan adoCIProbePlan) ([]int, error) {
	if plan.target.targeted {
		for _, pr := range eligible {
			if pr.Number == plan.target.number && adoCIProbeCanChangeTier(pr) {
				return []int{pr.Number}, nil
			}
		}
		return nil, nil
	}
	probeable := make([]providers.PullRequestSummary, 0, len(eligible))
	for _, pr := range eligible {
		if adoCIProbeCanChangeTier(pr) {
			probeable = append(probeable, pr)
		}
	}
	if len(probeable) == 0 {
		return nil, nil
	}
	available, err := plan.available(probeable)
	if err != nil {
		return nil, fmt.Errorf("filter claimed CI-probe candidates: %w", err)
	}
	numbers := make([]int, 0, len(available))
	for _, pr := range available {
		numbers = append(numbers, pr.Number)
	}
	sort.Ints(numbers)
	if len(numbers) == 0 {
		return nil, nil
	}
	window, err := plan.window(numbers)
	if err != nil {
		return nil, fmt.Errorf("advance CI-probe window: %w", err)
	}
	return window, nil
}

// adoCIProbeCanChangeTier reports whether reading pr's CI state could move it
// into the strong tier this cycle. A PR already labelled
// goobers:needs-remediation is strong regardless. A PR parked
// goobers:merge-escalated is out of the automated loop until a human clears
// that label — ADO has no head-moved self-heal for it the way GitHub's
// escalationStillBlocks does — so failing CI must not pull it back in.
func adoCIProbeCanChangeTier(pr providers.PullRequestSummary) bool {
	return pr.CheckState != providers.CheckStateFailing &&
		!hasAnyLabel(pr.Labels, []string{needsRemediationLabel}) &&
		!hasLabelFold(pr.Labels, remediationEscalatedLabel)
}

// adoCIProbeWindow picks at most limit of probeable (ascending PR numbers),
// starting at the first number after cursor and wrapping around, and returns
// the cursor for the next cycle: the last number this window took, or 0 when
// the whole set fit and there is nothing to rotate. Successive cycles
// therefore walk the whole set, so every probeable PR is read within
// ceil(len/limit) cycles however large the set, and a cycle that starts fresh
// (cursor 0) reads the oldest PRs first.
func adoCIProbeWindow(probeable []int, cursor, limit int) (window []int, next int) {
	if len(probeable) <= limit {
		return probeable, 0
	}
	start := sort.SearchInts(probeable, cursor+1)
	if start == len(probeable) {
		start = 0
	}
	window = make([]int, 0, limit)
	for i := 0; i < limit; i++ {
		window = append(window, probeable[(start+i)%len(probeable)])
	}
	return window, window[len(window)-1]
}

const (
	adoCIProbeCursorSchema        = "goobers.dev/pr-remediation-ci-probe/v1"
	adoCIProbeCursorLockOperation = "pr-remediation-ci-probe.update"
)

// adoCIProbeCursor is the persisted end of the last CI-probe window for one
// gaggle's repository.
type adoCIProbeCursor struct {
	After int `json:"after"`
}

var adoCIProbeCursorSpec = keyedStateRecordSpec[adoCIProbeCursor]{
	schema:      adoCIProbeCursorSchema,
	operation:   adoCIProbeCursorLockOperation,
	errorPrefix: "decode pr-remediation CI-probe cursor",
	field:       keyedStateRecordFieldRecord,
	stateKey:    adoCIProbeCursorStateKey,
}

// adoCIProbeCursorKey is the cursor's canonical identity: the gaggle and the
// repository, so two repositories (or gaggles) never share a window.
func adoCIProbeCursorKey(gaggle string, repo providers.RepositoryRef) string {
	return gaggle + "/" + string(repo.Provider) + "/" + repo.Owner + "/" + repo.Project + "/" + repo.Name
}

func adoCIProbeCursorStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.PRRemediationCIProbeKey(fmt.Sprintf("%x", sum))
}

// advanceADOCIProbeWindow reads this repository's CI-probe cursor, picks the
// cycle's window and records where it ended — one compare-and-swap on the
// plane, one claims.lock acquisition on the file backend. The window is
// assigned from the attempt that wins, so a retried compare-and-swap never
// probes a window it did not record. A cursor that cannot be read or written
// fails the stage, like every other scheduler-state read this stage makes: a
// cursor silently reset to 0 every cycle is exactly the starvation it exists
// to prevent.
func advanceADOCIProbeWindow(l instance.Layout, repo providers.RepositoryRef, probeable []int) ([]int, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nil, fmt.Errorf("open pr-remediation CI-probe cursor: %w", err)
	}
	var window []int
	key := adoCIProbeCursorKey(remediationNoopGaggle(l), repo)
	err = updateKeyedStateRecord(stateContext(), store, key, adoCIProbeCursorSpec,
		func(current adoCIProbeCursor, _ bool) (adoCIProbeCursor, bool, error) {
			var next int
			window, next = adoCIProbeWindow(probeable, current.After, adoRemediationCIProbeLimit)
			return adoCIProbeCursor{After: next}, next != current.After, nil
		})
	if err != nil {
		return nil, err
	}
	return window, nil
}
