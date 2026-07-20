package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// verdictSchemaVersion is bumped whenever apiv1.Verdict's shape or
// verdictJSONComment's payload encoding changes in a way that would make an
// OLD cached verdict misleading if silently reused (issue #523's ruling: "a
// verdict-schema version bump" is one of the four things that must force a
// fresh review). Folded into reviewDigest so a bump invalidates every
// standing cache entry without touching any PR's state.
const verdictSchemaVersion = 3

// computeSiblingSetHash returns an order-independent identity for every
// sibling the holistic reviewer sees. A sibling is identified by PR number
// and head SHA, so adding, removing, or updating one independently invalidates
// the verdict cache. An incomplete or duplicate entry makes the key unusable.
func computeSiblingSetHash(siblings []siblingPR) string {
	sorted := append([]siblingPR(nil), siblings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	h := sha256.New()
	for i, sibling := range sorted {
		if sibling.Number <= 0 || strings.TrimSpace(sibling.HeadSHA) == "" {
			return ""
		}
		if i > 0 && sorted[i-1].Number == sibling.Number {
			return ""
		}
		_, _ = fmt.Fprintf(h, "%d:%s\n", sibling.Number, sibling.HeadSHA)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// computeReviewDigest is merge-review's stable cross-run cache key:
// (selected head SHA, selected base SHA, sibling-set hash). Empty means at
// least one key component was unavailable and forces a fresh review.
func computeReviewDigest(selectedHeadSHA, selectedBaseSHA string, siblings []siblingPR) string {
	if strings.TrimSpace(selectedHeadSHA) == "" || strings.TrimSpace(selectedBaseSHA) == "" {
		return ""
	}
	siblingSetHash := computeSiblingSetHash(siblings)
	if siblingSetHash == "" {
		return ""
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v%d\nhead:%s\nbase:%s\nsiblings:%s\n", verdictSchemaVersion, selectedHeadSHA, selectedBaseSHA, siblingSetHash)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
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
	if !v.Decision.IsValid() ||
		v.Digest != wantDigest ||
		strings.TrimSpace(v.SourceRunID) == "" ||
		v.HeadSHA != wantHeadSHA ||
		v.BaseSHA != wantBaseSHA {
		return false
	}
	for _, finding := range v.Findings {
		if !finding.IsValid() {
			return false
		}
	}
	return true
}
