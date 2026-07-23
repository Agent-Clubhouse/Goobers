package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gooberruntime"
	"github.com/goobers/goobers/providers"
)

// verdictSchemaVersion is bumped whenever apiv1.Verdict's shape,
// verdictJSONComment's payload encoding, OR the reviewDigest key formula
// changes in a way that would make an OLD cached verdict misleading if silently
// reused (issue #523's ruling: "a verdict-schema version bump" is one of the
// four things that must force a fresh review). Folded into reviewDigest so a
// bump invalidates every standing cache entry without touching any PR's state.
//
// v5 (#1237): reviewDigest no longer folds in the open-PR sibling set — see
// computeReviewDigest. The bump forces one clean re-review of every parked PR
// onto the new relevance-scoped key rather than silently reinterpreting v4
// digests.
const verdictSchemaVersion = 5

// computeReviewDigest is merge-review's stable cross-run cache key for the
// SELECTED PR's own reviewable state: (schema version, selected head SHA,
// selected base SHA). Empty means head or base was unavailable, which forces a
// fresh review.
//
// It deliberately does NOT fold in the open-PR sibling set (#1237). Doing so
// meant any change to any sibling — overwhelmingly, an UNRELATED new PR opening
// elsewhere in a high-velocity batch (roughly one every few minutes) —
// invalidated this key, defeating the verdict cache entirely and re-deriving an
// unchanged blocked-on-sibling verdict on a ~30-minute treadmill for PRs whose
// own diff and named blockers never moved. The selected PR's own reviewable
// state is fully captured by its head and base SHAs: its own commits (head) and
// a sibling merge that actually rebased its effective base (base) both still
// invalidate. Sibling RELEVANCE is handled separately and precisely by
// cachedBlockerVerdictStillApplies, which invalidates a cached
// blocked-on-sibling verdict only when the specific sibling(s) it named as
// blockers resolve — never when unrelated siblings churn.
func computeReviewDigest(selectedHeadSHA, selectedBaseSHA string) string {
	if strings.TrimSpace(selectedHeadSHA) == "" || strings.TrimSpace(selectedBaseSHA) == "" {
		return ""
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v%d\nhead:%s\nbase:%s\n", verdictSchemaVersion, selectedHeadSHA, selectedBaseSHA)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// cachedBlockerVerdictStillApplies reports whether a cached blocked-on-sibling
// verdict is still accurate enough to reuse for the selected PR given the
// current open-PR set: the PR must still be blocked, i.e. at least one sibling
// the verdict named as a blocker (Finding.BlockingPRs on a cross-pr-blocked
// finding, #747) is still an open, non-merge-demoted PR. This mirrors
// blockedOnSiblingStillBlocks / liveBlockedOnSiblingBlockers exactly (#748/#950:
// a merged, closed, or merge-demoted blocker no longer holds a successor back).
//
// When every named blocker has resolved, the PR may now be free to progress, so
// the cached "blocked" verdict must NOT be reused — force a fresh review. A
// verdict that names no blockers (a pass, or a content needs-changes) always
// still applies here: its validity is fully captured by the head/base
// reviewDigest, and no amount of sibling churn changes it (#1237).
func cachedBlockerVerdictStillApplies(v apiv1.Verdict, siblings []siblingPR) bool {
	blockers := unionBlockingPRs(v.Findings)
	if len(blockers) == 0 {
		return true
	}
	stillBlocking := make(map[int]bool, len(siblings))
	for _, s := range siblings {
		stillBlocking[s.Number] = !hasAnyLabel(s.Labels, []string{mergeDemotedLabel})
	}
	for _, blocker := range blockers {
		if stillBlocking[blocker] {
			return true
		}
	}
	return false
}

type authenticatedBacklogProvider interface {
	providers.BacklogProvider
	AuthenticatedLogin(context.Context) (string, error)
}

// findCachedVerdict reads the canonical trusted merge-review status comment on
// selectedNumber and returns it only when its recorded Digest matches
// wantDigest and its SHA pins, decision, findings, and source-run provenance
// are usable. Any incomplete or mismatched prior result is not a cache hit,
// so the caller falls through to a real review.
func findCachedVerdict(ctx context.Context, provider authenticatedBacklogProvider, repo providers.RepositoryRef, selectedNumber int, wantDigest, wantHeadSHA, wantBaseSHA string) (*apiv1.Verdict, error) {
	if wantDigest == "" || wantHeadSHA == "" || wantBaseSHA == "" {
		return nil, nil
	}
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve merge-review verdict author: %w", err)
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return nil, fmt.Errorf("list comments on PR #%d: %w", selectedNumber, err)
	}
	for _, comment := range comments {
		if !isTrustedMergeReviewStatusComment(comment.Author, comment.Body, author) {
			continue
		}
		v, ok := parseVerdictComment(comment.Body)
		if !ok {
			return nil, nil
		}
		if cachedVerdictUsable(v, wantDigest, wantHeadSHA, wantBaseSHA) {
			return &v, nil
		}
		return nil, nil
	}
	return nil, nil
}

func cachedVerdictUsable(v apiv1.Verdict, wantDigest, wantHeadSHA, wantBaseSHA string) bool {
	if gooberruntime.ValidateMergeReviewVerdict(v) != nil ||
		v.Digest != wantDigest ||
		strings.TrimSpace(v.SourceRunID) == "" ||
		v.HeadSHA != wantHeadSHA ||
		v.BaseSHA != wantBaseSHA {
		return false
	}
	return true
}
