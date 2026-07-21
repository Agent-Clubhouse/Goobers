package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/goobers/goobers/providers"
)

// mergeDemotedLabel marks a crowned lander that repeatedly could not merge at
// the same head, so the election stops re-crowning it and its blocked cluster
// can drain AROUND it instead of deadlocking behind it (#950). The election is
// a pure function of {PR number, blockers}, so a FIFO-minimum lander that can
// never merge (red CI it recovers-then-loses, a base advance that keeps
// intersecting its files, an unresolvable conflict) is re-elected identically
// every cycle while every sibling parks blocked-on-sibling behind it — zero
// forward progress until a human intervenes. Demotion breaks that: after a
// bounded number of merge refusals at an unchanged head, the lander is excluded
// from election and its predecessors-liveness, so the next-lowest cluster
// member is crowned instead.
//
// Unlike goobers:merge-escalated (a human-only bucket, CLAUDE.md standing
// rule), demotion SELF-HEALS: the moment the PR's head advances past the
// recorded snapshot (a remediation push, a rebase — a genuinely new attempt),
// the demotion lifts and the PR is eligible to win its own election again. The
// demoted PR still goes through pr-remediation on its own merits; demotion is
// about unblocking its siblings, not abandoning it.
const mergeDemotedLabel = "goobers:merge-demoted"

// mergeDemotionState is the durable per-PR record the record-merge-refusal
// stage posts and the election read-sites consult — the merge-side analog of
// remediationState (remediationcheckpoint.go) and blockedOnSiblingState
// (applyverdict.go). It lives in a sticky PR comment so it survives across
// merge-review cycles without inventing a second source of truth about which
// PR is demoted (the same discipline #716/#748 use).
type mergeDemotionState struct {
	// Attempts counts consecutive merge refusals observed at HeadSHA. A refusal
	// at a NEW head resets it to 1 (the head moved, so this is a genuinely fresh
	// attempt), so only a stuck PR — one that cannot merge and whose head is not
	// advancing — ever accumulates toward the demotion threshold.
	Attempts int `json:"attempts"`
	// Demoted is true once Attempts crossed the threshold and the label was
	// applied. Recorded so a labeled-but-snapshotless PR (hand-labeled, or
	// labeled before this fix shipped) is handled the same fail-closed way
	// escalationStillBlocks handles its own missing snapshot.
	Demoted bool `json:"demoted"`
	// HeadSHA pins the PR head these refusals were observed at. The demotion
	// self-heals the instant the live head no longer matches (#950).
	HeadSHA string `json:"headSha"`
	// Reason is the latest merge refusal reason, for a human reading the thread.
	Reason string `json:"reason,omitempty"`
	// RecordedAt is when this record was posted.
	RecordedAt time.Time `json:"recordedAt"`
}

// mergeDemotionPattern matches the machine-readable payload
// mergeDemotionComment appends — mirrors remediationStatePattern /
// blockedOnSiblingPattern.
var mergeDemotionPattern = regexp.MustCompile(`(?s)<!-- merge-demotion-state: (.*?) -->`)

func mergeDemotionComment(s mergeDemotionState) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal merge-demotion payload: %w", err)
	}
	return fmt.Sprintf("<!-- merge-demotion-state: %s -->", data), nil
}

func parseMergeDemotionComment(body string) (mergeDemotionState, bool) {
	m := mergeDemotionPattern.FindStringSubmatch(body)
	if m == nil {
		return mergeDemotionState{}, false
	}
	var s mergeDemotionState
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return mergeDemotionState{}, false
	}
	return s, true
}

// latestMergeDemotionState returns the most recent demotion record in the
// thread plus its comment ID (so the recorder can update it in place rather
// than growing a second channel). Mirrors latestRemediationState /
// latestBlockedOnSiblingState.
func latestMergeDemotionState(comments []providers.Comment) (state mergeDemotionState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := parseMergeDemotionComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return mergeDemotionState{}, "", false
}

// demotionStillHolds reports whether pr is currently demoted (#950): it carries
// goobers:merge-demoted AND the recorded snapshot's head still matches the PR's
// live head. A head advance past the snapshot self-heals the demotion (a fresh
// election shot), exactly as escalationStillBlocks self-heals a merge-escalated
// PR on base advance. Fetches comments only for a labeled PR (a small, by-design
// subset), so this stays cheap for the common unlabeled case.
func demotionStillHolds(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	if !hasAnyLabel(pr.Labels, []string{mergeDemotedLabel}) {
		return false, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	state, _, found := latestMergeDemotionState(comments)
	if !found || !state.Demoted {
		// Labeled but no recorded demotion snapshot — a PR labeled by hand or
		// before this fix. Fail closed (still demoted) until a human clears the
		// label, since there is no snapshot to self-heal against.
		return true, nil
	}
	if state.HeadSHA != pr.HeadSHA {
		// Head advanced past the demotion snapshot — a genuinely fresh attempt.
		return false, nil
	}
	return true, nil
}

// demotedSet returns the subset of the given open PRs that are currently
// demoted, snapshot-validated. Callers already hold the open-PR list (pr-select
// and elect-lander both list it), so this adds only a ListComments per
// currently-labeled PR. Per #950's fail-safe contract the CALLER decides what
// to do with an error: the election read-sites treat a resolution failure as an
// empty demoted set (today's behavior) rather than failing the pipeline, so a
// provider hiccup can never turn the demotion signal into a merge outage.
func demotedSet(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary) (map[int]bool, error) {
	out := map[int]bool{}
	for _, pr := range prs {
		if !hasAnyLabel(pr.Labels, []string{mergeDemotedLabel}) {
			continue
		}
		held, err := demotionStillHolds(ctx, provider, repo, pr)
		if err != nil {
			return nil, err
		}
		if held {
			out[pr.Number] = true
		}
	}
	return out, nil
}

// withoutDemoted removes any demoted PR from a blocker list. Both election
// read-sites (elect-lander's electionDecision and apply-verdict's
// predecessorBlockers/electedLanderPass) apply it identically so the two stages
// never disagree about which sibling a PR must wait behind (#950): a demoted
// predecessor no longer outranks its successors, so the next-lowest
// non-demoted member wins the election and the cluster drains around the stuck
// one.
func withoutDemoted(blockers []int, demoted map[int]bool) []int {
	if len(demoted) == 0 {
		return blockers
	}
	out := make([]int, 0, len(blockers))
	for _, b := range blockers {
		if demoted[b] {
			continue
		}
		out = append(out, b)
	}
	return out
}
