package remediationselect

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// BlockedOnSiblingLabel marks a PR that's correct in isolation but must wait
// behind a named sibling (#747) — see verdictLabel's doc comment.
const BlockedOnSiblingLabel = providers.LabelBlockedOnSibling

// MergeReviewStatusMarker begins a merge-review status comment.
const MergeReviewStatusMarker = "<!-- goobers:merge-review-status -->"

// UnionBlockingPRs collects the deduplicated, sorted union of BlockingPRs
// across every finding — a verdict can carry more than one cross-pr-blocked
// finding (e.g. two independent ordering asks against two different
// siblings), and blockedOnSiblingState.Blockers records the full set, not
// just the first finding's.
func UnionBlockingPRs(findings []apiv1.Finding) []int {
	seen := make(map[int]bool)
	var out []int
	for _, f := range findings {
		for _, pr := range f.BlockingPRs {
			if !seen[pr] {
				seen[pr] = true
				out = append(out, pr)
			}
		}
	}
	sort.Ints(out)
	return out
}

// BlockedOnSiblingState is the PR-altitude analog of blockedrecords.go's
// backlog-altitude blockedRecord (#747) — the structured record apply-verdict
// posts when a verdict's findings are entirely cross-PR-ordering asks. This
// is the source of truth #748's selection-exclusion/self-heal reads: which
// PR(s) this one is genuinely waiting behind, so it can be excluded from
// re-selection until they close and unparked once they do — without that
// consulting a full Verdict's Findings array.
type BlockedOnSiblingState struct {
	// Blockers is the union of BlockingPRs across every cross-pr-blocked
	// finding in the verdict that produced this record.
	Blockers []int `json:"blockers"`
	// Reason is the verdict's own rationale, for a human reading the comment.
	Reason string `json:"reason"`
	// HeadSHA/BaseSHA pin the PR state this record was computed against —
	// same SHA-pinning discipline as Verdict's own HeadSHA/BaseSHA (design
	// doc §6 D6).
	HeadSHA string `json:"headSha"`
	BaseSHA string `json:"baseSha"`
	// RecordedAt is when this record was posted.
	RecordedAt time.Time `json:"recordedAt"`
}

// BlockedOnSiblingPattern matches the machine-readable payload
// blockedOnSiblingComment appends — mirrors verdictJSONPattern above.
var BlockedOnSiblingPattern = regexp.MustCompile(`(?s)<!-- blocked-on-sibling: (.*?) -->`)

// ParseBlockedOnSiblingComment recovers the blockedOnSiblingState a
// prior apply-verdict run embedded in a PR comment — the read side #748's
// selection-exclusion/self-heal uses. Returns ok=false if body has no
// embedded payload, the normal case for any comment apply-verdict didn't
// post as blocked-on-sibling.
func ParseBlockedOnSiblingComment(body string) (s BlockedOnSiblingState, ok bool) {
	m := BlockedOnSiblingPattern.FindStringSubmatch(body)
	if m == nil {
		return BlockedOnSiblingState{}, false
	}
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return BlockedOnSiblingState{}, false
	}
	return s, true
}

// IsMergeReviewStatusComment reports whether body is a merge-review status comment.
func IsMergeReviewStatusComment(body string) bool {
	return body == MergeReviewStatusMarker || strings.HasPrefix(body, MergeReviewStatusMarker+"\n")
}

// VerdictJSONPattern matches the machine-readable payload
// renderVerdictComment appends to its posted comment.
var VerdictJSONPattern = regexp.MustCompile(`(?s)<!-- verdict-json: (.*?) -->`)

// ParseVerdictComment recovers the Verdict a merge-review apply-verdict run
// embedded in a PR comment via verdictJSONComment — the handoff
// pr-remediation's gather-pr-context (issue #362) uses to read merge-review's
// structured verdict back from a DIFFERENT run's own journal (which has no
// artifact for it). Returns ok=false if body has no embedded payload (an
// older comment, or one not posted by apply-verdict at all) — that is a
// normal "no verdict recorded yet" outcome, not a parse error.
func ParseVerdictComment(body string) (v apiv1.Verdict, ok bool) {
	m := VerdictJSONPattern.FindStringSubmatch(body)
	if m == nil {
		return apiv1.Verdict{}, false
	}
	if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
		return apiv1.Verdict{}, false
	}
	return v, true
}
