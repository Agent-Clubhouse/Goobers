package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// verdictSchemaVersion is bumped whenever apiv1.Verdict's shape or
// verdictJSONComment's payload encoding changes in a way that would make an
// OLD cached verdict misleading if silently reused (issue #523's ruling: "a
// verdict-schema version bump" is one of the four things that must force a
// fresh review). Folded into reviewDigest so a bump invalidates every
// standing cache entry without touching any PR's state.
const verdictSchemaVersion = 1

// computeReviewDigest is merge-review's cross-run cache key (issue #523's
// maintainer ruling): a content hash of every input the holistic reviewer's
// verdict actually depends on — the selected PR's own head+base SHAs, plus
// every sibling's (number, head SHA) pair, plus the schema version. Two
// gathers with an identical digest saw byte-identical review inputs, so a
// verdict computed for one is exactly as valid for the other. Siblings are
// sorted by number first so gather order (a map/slice iteration artifact,
// never semantically meaningful) can never perturb the digest.
func computeReviewDigest(selectedHeadSHA, selectedBaseSHA string, siblings []siblingPR) string {
	sorted := append([]siblingPR(nil), siblings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	h := sha256.New()
	// hash.Hash.Write never returns an error (per its doc comment) — the
	// error return exists only to satisfy io.Writer.
	_, _ = fmt.Fprintf(h, "v%d\nhead:%s\nbase:%s\n", verdictSchemaVersion, selectedHeadSHA, selectedBaseSHA)
	for _, s := range sorted {
		_, _ = fmt.Fprintf(h, "sibling:%d:%s\n", s.Number, s.HeadSHA)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// findCachedVerdict looks up the most recently posted verdict comment on
// selectedNumber (the same verdictJSONComment payload apply-verdict already
// posts, parsed by parseVerdictComment — gather-pr-context's exact
// mechanism for reading a merge-review verdict back from a DIFFERENT run's
// comments, reused here rather than duplicated) and returns it only when
// its recorded Digest matches wantDigest. A prior verdict with no Digest
// (posted before #523, or by a schema version this instance no longer
// trusts) or a non-matching Digest is not a cache hit — the caller falls
// through to a real review, exactly as if no comment existed at all.
func findCachedVerdict(ctx context.Context, provider providers.BacklogProvider, repo providers.RepositoryRef, selectedNumber int, wantDigest string) (*apiv1.Verdict, error) {
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return nil, fmt.Errorf("list comments on PR #%d: %w", selectedNumber, err)
	}
	for i := len(comments) - 1; i >= 0; i-- {
		v, ok := parseVerdictComment(comments[i].Body)
		if !ok {
			continue
		}
		if v.Digest != "" && v.Digest == wantDigest {
			return &v, nil
		}
		// The latest verdict comment is the only one that could possibly
		// still be valid (an older one is superseded regardless of its own
		// digest) — stop at the first one found, matching or not.
		return nil, nil
	}
	return nil, nil
}
