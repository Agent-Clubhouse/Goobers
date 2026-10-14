package prstatus

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/goobers/goobers/providers"
)

// The merge-review verdict status (#4199) is the native, merge-blocking form
// of merge-review's verdict: apply-verdict publishes it on the exact head it
// reviewed, and repositories make its context a required status check so
// no merge path (a human, native auto-merge, Goobers' own merge-pr) can land
// a pull request the authoritative reviewer has not passed.
//
// The status is pinned to the head by construction, because a commit status
// belongs to one commit: a verdict for an older head never reaches a newer
// one, which carries no verdict status until it is reviewed. The base is
// pinned through the description, which always begins
// "<decision> for base <sha>"; merge-pr refuses a pass recorded for any base
// but the one its own review pinned, and apply-verdict replaces the status
// with a pending "stale" one when it finds the base has moved under a
// standing verdict.

// verdictDescriptionPattern recovers the decision and reviewed base from a
// verdict status description.
var verdictDescriptionPattern = regexp.MustCompile(`^(\S+) for base (\S+)`)

// VerdictState maps a published verdict decision to the status state that
// enforces it: pass succeeds; needs-changes, fail and escalate fail; defer
// (waiting on a sibling, not a defect) stays pending. Every state but
// success blocks a required check.
//
// Decisions are the published verdict decision strings (api/v1alpha1's
// VerdictDecision values); this package stays free of the wire contract.
func VerdictState(decision string) (providers.CheckState, error) {
	switch decision {
	case "pass":
		return providers.CheckStatePassing, nil
	case "needs-changes", "fail", "escalate":
		return providers.CheckStateFailing, nil
	case "defer":
		return providers.CheckStatePending, nil
	default:
		return "", fmt.Errorf("unsupported verdict decision %q", decision)
	}
}

// VerdictDescription renders the status description: the decision and the
// reviewed base, then a one-line summary. Providers bound its length.
//
// The description is bounded to DescriptionLimit runes here rather than by
// each provider, so the description a provider stores is exactly the one
// rendered, and an unchanged verdict can be recognized and not re-posted.
func VerdictDescription(decision, baseSHA, summary string) string {
	description := fmt.Sprintf("%s for base %s", decision, strings.TrimSpace(baseSHA))
	if line := strings.Join(strings.Fields(summary), " "); line != "" {
		description += ": " + line
	}
	if runes := []rune(description); len(runes) > DescriptionLimit {
		description = string(runes[:DescriptionLimit-1]) + "…"
	}
	return description
}

// DescriptionLimit is the longest verdict status description: GitHub's
// commit-status bound, the tightest of the supported providers.
const DescriptionLimit = 140

// VerdictStatusRequest builds the status publishing decision for the pull
// request at headSHA, reviewed against baseSHA.
func VerdictStatusRequest(
	repo providers.RepositoryRef,
	pullID string,
	decision string,
	headSHA, baseSHA, summary, targetURL string,
) (providers.PullRequestStatusRequest, error) {
	state, err := VerdictState(decision)
	if err != nil {
		return providers.PullRequestStatusRequest{}, err
	}
	if strings.TrimSpace(headSHA) == "" || strings.TrimSpace(baseSHA) == "" {
		return providers.PullRequestStatusRequest{}, fmt.Errorf("a verdict status needs the reviewed head and base")
	}
	return providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      pullID,
		Genre:       providers.MergeReviewStatusGenre,
		Name:        providers.MergeReviewStatusName,
		State:       state,
		Description: VerdictDescription(decision, baseSHA, summary),
		TargetURL:   targetURL,
		HeadSHA:     headSHA,
	}, nil
}

// StaleVerdictStatusRequest builds the pending status that withdraws a
// standing verdict on headSHA once the review it recorded no longer stands
// (the base moved, or the reviewer's pin disagreed). Pending blocks a
// required check until merge-review publishes a fresh verdict.
func StaleVerdictStatusRequest(repo providers.RepositoryRef, pullID, headSHA, baseSHA, reason, targetURL string) providers.PullRequestStatusRequest {
	return providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      pullID,
		Genre:       providers.MergeReviewStatusGenre,
		Name:        providers.MergeReviewStatusName,
		State:       providers.CheckStatePending,
		Description: VerdictDescription("stale", baseSHA, "re-review pending; "+reason),
		TargetURL:   targetURL,
		HeadSHA:     headSHA,
	}
}

// ParseVerdictDescription recovers the decision and reviewed base a verdict
// status description records.
func ParseVerdictDescription(description string) (decision, baseSHA string, ok bool) {
	match := verdictDescriptionPattern.FindStringSubmatch(strings.TrimSpace(description))
	if match == nil {
		return "", "", false
	}
	return match[1], strings.TrimSuffix(match[2], ":"), true
}

// VerdictStatusRefusal returns why the merge-review verdict status on the
// polled head forbids landing a review pinned to baseSHA, or "" when it
// permits it. A head without the status is not refused here: the provider's
// required check refuses that natively, and a credential that cannot publish
// statuses must not stop merge-pr, whose other conjuncts still hold.
func VerdictStatusRefusal(status *providers.CheckDetail, headSHA, baseSHA string) string {
	if status == nil {
		return ""
	}
	prefix := fmt.Sprintf("merge-review verdict status %s on head %s", providers.MergeReviewStatusContext, headSHA)
	switch status.State {
	case providers.CheckStatePassing:
		_, base, ok := ParseVerdictDescription(status.Summary)
		if !ok {
			return prefix + " is a pass that names no reviewed base — not an authoritative verdict (#4199)"
		}
		if base != baseSHA {
			return fmt.Sprintf("%s is a pass for base %s, but this landing is pinned to base %s — not the current verdict (#4199)", prefix, base, baseSHA)
		}
		return ""
	case providers.CheckStateFailing:
		return fmt.Sprintf("%s is failure (%s) — a non-pass merge-review verdict blocks landing (#4199)", prefix, status.Summary)
	default:
		return fmt.Sprintf("%s is pending (%s) — no current pass verdict (#4199)", prefix, status.Summary)
	}
}

// VerdictSummary is the one-line reason a verdict status shows: the
// verdict's summary, else the first line of its rationale.
func VerdictSummary(summary, rationale string) string {
	if summary := strings.TrimSpace(summary); summary != "" {
		return summary
	}
	line, _, _ := strings.Cut(strings.TrimSpace(rationale), "\n")
	return line
}

// VerdictPublication reports what PublishVerdict or WithdrawVerdict did when
// it returned no error.
type VerdictPublication struct {
	// Warning, when set, says why the status was not published and the
	// verdict is therefore not enforced by the provider's merge policy.
	Warning string
	// Unchanged means nothing needed posting: the same state and
	// description already stand on the head, or (withdrawal) no verdict
	// status stands there at all.
	Unchanged bool
}

// PublishVerdict posts a verdict status through publisher.
//
// It is idempotent: when the publisher can read the head's standing verdict
// status and it already has this state and description, nothing is posted.
// A head re-reviewed every tick would otherwise accumulate a status per run
// until GitHub's per-context cap refused the next one.
//
// A provider that cannot publish statuses, a credential refused the status
// write, or a commit at the per-context status cap degrades to a warning
// rather than failing the verdict: the same identity publishes every verdict
// status, so one that cannot write statuses never published a pass either,
// and a required check without any status already blocks natively. Any other
// failure is returned, so a verdict is never reported published while its
// status is missing.
func PublishVerdict(ctx context.Context, publisher any, req providers.PullRequestStatusRequest) (VerdictPublication, error) {
	return publishVerdict(ctx, publisher, req, false)
}

// WithdrawVerdict posts req (a StaleVerdictStatusRequest) only when a verdict
// status stands on the head: a head with no verdict status needs no
// withdrawal, and a pending status there would only add noise on a
// repository that does not require the check. When the standing status
// cannot be read it is withdrawn anyway, which can only block.
func WithdrawVerdict(ctx context.Context, publisher any, req providers.PullRequestStatusRequest) (VerdictPublication, error) {
	return publishVerdict(ctx, publisher, req, true)
}

func publishVerdict(ctx context.Context, publisher any, req providers.PullRequestStatusRequest, onlyIfStanding bool) (VerdictPublication, error) {
	statusContext := req.Genre + "/" + req.Name
	statusPublisher, ok := publisher.(providers.PullRequestStatusPublisher)
	if !ok {
		return VerdictPublication{Warning: fmt.Sprintf("provider cannot publish the %s status, so this verdict is not enforced by the repository's merge policy", statusContext)}, nil
	}
	if reader, ok := publisher.(providers.MergeReviewStatusReader); ok && statusContext == providers.MergeReviewStatusContext {
		if current, err := reader.MergeReviewStatus(ctx, req.Repository, req.HeadSHA); err == nil {
			if current == nil && onlyIfStanding {
				return VerdictPublication{Unchanged: true}, nil
			}
			if current != nil && current.State == req.State && current.Summary == req.Description {
				return VerdictPublication{Unchanged: true}, nil
			}
		}
	}
	if _, err := statusPublisher.PublishPullRequestStatus(ctx, req); err != nil {
		switch {
		case providers.IsStatusPublishDeniedError(err):
			return VerdictPublication{Warning: fmt.Sprintf("the review credential may not write commit statuses, so %s was not published and this verdict is not enforced by the repository's merge policy; grant it Commit statuses: Read and write (#4199): %v", statusContext, err)}, nil
		case providers.IsStatusLimitError(err):
			return VerdictPublication{Warning: fmt.Sprintf("head %s has reached the provider's cap on %s statuses, so this verdict was not published and the latest accepted status still stands: %v", req.HeadSHA, statusContext, err)}, nil
		}
		return VerdictPublication{}, fmt.Errorf("publish %s status: %w", statusContext, err)
	}
	return VerdictPublication{}, nil
}
