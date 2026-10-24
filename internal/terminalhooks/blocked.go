package terminalhooks

import (
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// BlockedReason condenses a blocked result's own explanation for the journal's
// blocked_by_agent cause event: the structured error detail when present
// (code-prefixed, so an agent's DEPENDENCY_NOT_MET survives into the run-level
// record), else the summary, else a fixed marker so the event is never empty.
func BlockedReason(result apiv1.ResultEnvelope) string {
	if result.Error != nil && result.Error.Message != "" {
		if result.Error.Code != "" {
			return result.Error.Code + ": " + result.Error.Message
		}
		return result.Error.Message
	}
	if s := strings.TrimSpace(result.Summary); s != "" {
		return s
	}
	return "stage reported blocked with no error detail"
}

// ParseBlockedBy extracts blocking issue numbers from the documented
// outputs.blockedBy convention (docs/stage-contract.md): a scalar string of
// comma-separated issue numbers — the envelope schema admits only scalars in
// outputs, which is exactly why live blocked results that tried a structured
// array got schema-rejected and burned an attempt (#545). Lenient on input
// shape (tolerates "#" prefixes, whitespace, a bare JSON number), strict on
// content: only all-digit tokens survive, deduplicated in first-seen order.
// Nil when the key is absent or nothing parseable remains.
func ParseBlockedBy(outputs map[string]interface{}) []string {
	v, ok := outputs[OutputBlockedBy]
	if !ok || v == nil {
		return nil
	}
	var raw string
	switch n := v.(type) {
	case string:
		raw = n
	case float64:
		raw = strconv.FormatInt(int64(n), 10)
	case int:
		raw = strconv.Itoa(n)
	default:
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		tok = strings.TrimPrefix(strings.TrimSpace(tok), "#")
		if tok == "" || seen[tok] {
			continue
		}
		allDigits := true
		for _, r := range tok {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// OutputBlockedBy is the documented ResultEnvelope output key a blocked stage
// references its blocking issue numbers through — comma-separated numbers in
// a single scalar string (outputs are scalar-only by schema). Shared by the
// runner's parse (above) and the instructions/docs that teach producers the
// convention.
const OutputBlockedBy = "blockedBy"

// SelfBlockerDroppedKind tags the runner.annotation recording that a stage
// named its own driving item in outputs.blockedBy and the self-reference was
// dropped before persistence (#2961).
const SelfBlockerDroppedKind = "blocked_by.self_reference_dropped"

// FilterSelfBlockers removes blockers naming the driving item itself, which an
// item can never depend on (#2961). Admitting a self-reference writes a
// one-node self-edge into scheduler/blocked.json that the cycle detector then
// correctly reports as a circular dependency, parking the issue for human
// resolution over a dependency that does not exist. The detector is not the
// bug — the missing validation at the point model-authored blocker output is
// admitted is, so this filters on the way in and leaves persisted-graph
// self-loop handling intact for legacy or corrupt records.
//
// Both sides are normalized (repository qualifier and "#" prefix stripped) so
// "#441", "owner/repo#441" and "441" all match item 441. Matching is exact
// after normalization: a pull-request item ("pr/536") is never considered
// self-blocked by issue 536, which is a legitimate dependency.
// Returns the blockers to keep and those dropped, in first-seen order.
func FilterSelfBlockers(blockers []string, itemID string) (kept, dropped []string) {
	self := normalizeBlockerToken(itemID)
	if self == "" {
		return blockers, nil
	}
	for _, blocker := range blockers {
		if normalizeBlockerToken(blocker) == self {
			dropped = append(dropped, blocker)
			continue
		}
		kept = append(kept, blocker)
	}
	return kept, dropped
}

// normalizeBlockerToken reduces an item id or blocker reference to the bare
// identifier both sides of a self-comparison can be keyed on, mirroring the
// CLI's normalizeBlockedReference: drop any repository qualifier ahead of the
// last "#", then any leading "#".
func normalizeBlockerToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if separator := strings.LastIndex(raw, "#"); separator >= 0 {
		raw = raw[separator+1:]
	}
	return strings.TrimSpace(strings.TrimPrefix(raw, "#"))
}
