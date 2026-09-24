package harness

import (
	"bytes"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

// ErrorCodeToolPermissionDenied is the distinct failure code a stage carries
// when the harness observed the CLI refusing a tool call for permission
// reasons (#2962). It is deliberately its own code rather than a generic
// failure: the operator action is "grant the tool / fix the harness
// invocation", which is nothing like the product decision an unattributed
// block asks a human for.
const ErrorCodeToolPermissionDenied = "HARNESS_TOOL_PERMISSION_DENIED"

// ErrorCodeUnsubstantiatedContentExclusion is the failure code for a
// model-authored organization content-exclusion classification the runtime
// never signalled (#2962). Content exclusion is an organization policy fact,
// not something an agent may infer from a tool call it could not make.
const ErrorCodeUnsubstantiatedContentExclusion = "UNSUBSTANTIATED_CONTENT_EXCLUSION"

// toolPermissionMarkers are the runtime phrases that positively identify a
// generic tool-permission refusal. These come from the CLI/runtime, never
// from model prose: matching them is evidence, not inference.
var toolPermissionMarkers = []string{
	"permission denied and could not request permission from user",
	"tool permission denied",
	"permission to run tool",
	"is not allowed to run tool",
	"tool call was denied",
}

// contentExclusionRuntimeMarkers are the phrases that constitute an EXPLICIT
// runtime content-exclusion signal. Only these keep a content-exclusion
// classification alive; the model asserting it in prose does not.
//
// Kept narrow on purpose: each names the policy mechanism itself
// ("content exclusion", the repository/organization policy that hides paths
// from Copilot), not the generic vocabulary of refusal that any denied tool
// call produces.
var contentExclusionRuntimeMarkers = []string{
	"content exclusion",
	"content_exclusion",
	"content-exclusion rule",
	"excluded by your organization",
	"excluded by the repository owner",
	"blocked by content exclusion",
	"copilot is disabled for this file",
}

// contentExclusionClaimMarkers are the ways a model states the conclusion
// this guard refuses to take on faith. Matched against the result envelope's
// own prose (summary, error code/message, scalar outputs) — never against the
// transcript, where the same words may legitimately appear as the runtime
// signal detected above.
var contentExclusionClaimMarkers = []string{
	"content exclusion",
	"content_exclusion",
	"content-exclusion",
	"contentexclusion",
	"excluded by your organization",
	"excluded by the repository owner",
	"organization policy blocks",
	"org content policy",
}

// evidenceGatedClaimMarkers are phrasings real #5444 transcripts used to
// assert the same conclusion as contentExclusionClaimMarkers without using
// the words "content exclusion" (e.g. CANONICAL_SPEC_ACCESS_DENIED: "...
// was denied by the environment access policy", PINNED_SPEC_ACCESS_DENIED).
//
// Unlike contentExclusionClaimMarkers, these may NEVER complete a claim on
// their own — they are also the exact vocabulary a genuine cloud-provider
// permission denial uses (a real Key Vault, S3, or IAM 403). An independent
// review (review DA, 2026-09-24) reproduced KEYVAULT_FORBIDDEN and
// S3_ACCESS_DENIED failures being misclassified as unsubstantiated
// content-exclusion claims when these were folded into the unconditional
// marker list. matchesClaimMarkers is only called against this list
// alongside positive runtime tool-refusal evidence (ev.denied) — see
// reclassifyToolPermissionBlock.
var evidenceGatedClaimMarkers = []string{
	"access_denied",
	"access policy",
}

// nonRetryableEscalationCodes mirrors internal/runner's escalateErrorCodes
// (#415): the recognized non-retryable business dispositions a stage can
// author to bypass the Next gate's repass loop entirely. This guard must
// never relabel one of these, even when its prose also happens to mention
// content exclusion or an access-denied phrase, and even when a genuine tool
// refusal also appears elsewhere in the same session's transcript —
// overwriting the code would silently turn a deliberate #415 escalation
// (ISSUE_OVER_SCOPE, NEEDS_DECOMPOSITION, ISSUE_NOT_APPLICABLE,
// SHARED_BASELINE_FAILURE) into an ordinary failure route.
//
// Duplicated here rather than imported from internal/runner, which this
// package must not depend on: harness produces result envelopes, runner
// interprets them, and runner already imports harness.
var nonRetryableEscalationCodes = map[string]bool{
	"ISSUE_OVER_SCOPE":                  true,
	"NEEDS_DECOMPOSITION":               true,
	telemetry.ErrCodeIssueNotApplicable: true,
	"SHARED_BASELINE_FAILURE":           true,
}

// toolPermissionEvidence is what the harness itself observed about tool
// permissions during a session, as distinct from what the model said about it.
type toolPermissionEvidence struct {
	// denied reports that at least one runtime tool-permission refusal was
	// observed in the captured bytes.
	denied bool
	// contentExclusionSignalled reports an EXPLICIT runtime content-exclusion
	// signal — the only thing that substantiates a content-exclusion block.
	contentExclusionSignalled bool
	// quotes are short, de-duplicated excerpts of the refusal lines, for the
	// operator-facing failure message. Bounded so a pathological transcript
	// cannot inflate a result envelope.
	quotes []string
}

const (
	maxPermissionQuotes    = 3
	maxPermissionQuoteRune = 200
)

// observeToolPermissions scans the bytes the harness captured (transcript and
// stderr) for runtime tool-permission and content-exclusion signals. Matching
// is case-insensitive and line-oriented so a structured JSON transcript and a
// plain-text one behave identically.
func observeToolPermissions(captures ...[]byte) toolPermissionEvidence {
	var ev toolPermissionEvidence
	seen := map[string]bool{}
	for _, capture := range captures {
		if len(capture) == 0 {
			continue
		}
		for _, rawLine := range bytes.Split(capture, []byte("\n")) {
			line := strings.TrimSpace(string(rawLine))
			if line == "" {
				continue
			}
			lower := strings.ToLower(line)
			if containsAny(lower, contentExclusionRuntimeMarkers) {
				ev.contentExclusionSignalled = true
			}
			if !containsAny(lower, toolPermissionMarkers) {
				continue
			}
			ev.denied = true
			if len(ev.quotes) >= maxPermissionQuotes {
				continue
			}
			quote := truncateRunes(line, maxPermissionQuoteRune)
			if seen[quote] {
				continue
			}
			seen[quote] = true
			ev.quotes = append(ev.quotes, quote)
		}
	}
	return ev
}

// matchesClaimMarkers reports whether the result envelope's own prose
// contains any of markers. Scans summary, error code and message, and scalar
// string outputs — the places a model actually states a conclusion,
// including outputs.blockedBy, where a live run wrote the free text
// "content-exclusion-policy" instead of the documented issue numbers.
func matchesClaimMarkers(result apiv1.ResultEnvelope, markers []string) bool {
	fields := []string{result.Summary}
	if result.Error != nil {
		fields = append(fields, result.Error.Code, result.Error.Message)
	}
	for _, v := range result.Outputs {
		if s, ok := v.(string); ok {
			fields = append(fields, s)
		}
	}
	for _, field := range fields {
		if containsAny(strings.ToLower(field), markers) {
			return true
		}
	}
	return false
}

// claimsContentExclusion reports whether the result envelope's own prose
// asserts an organization content-exclusion block, using only the
// unconditional marker set. See matchesClaimMarkers.
func claimsContentExclusion(result apiv1.ResultEnvelope) bool {
	return matchesClaimMarkers(result, contentExclusionClaimMarkers)
}

// reclassifyToolPermissionBlock enforces #2962's separation: a generic tool
// permission failure is a harness/infrastructure fault the operator can fix,
// while organization content exclusion is a policy fact. Left conflated, a
// denied tool call became a model-authored content-exclusion block, which
// parked the driving issue for a human — and re-parked it within the hour of
// every unpark, because nothing about the run had actually changed.
//
// Two conversions, both requiring the absence of an explicit runtime
// content-exclusion signal (when the runtime does signal exclusion, the
// classification is substantiated and is left exactly as authored):
//
//  1. Positive runtime evidence of a tool-permission refusal (ev.denied) turns
//     the result into a failure carrying ErrorCodeToolPermissionDenied and the
//     observed refusal lines, so the journal names the real fault. This is
//     the only conversion a "failure" status can take (#5444 review DA):
//     applies to BOTH "blocked" and "failure" statuses.
//  2. An unsubstantiated content-exclusion claim with NO runtime evidence at
//     all turns a "blocked" result — never a "failure" one — into a failure
//     carrying ErrorCodeUnsubstantiatedContentExclusion, because a model may
//     not assert an organization policy the runtime never reported. This
//     never invents a cause: the model's own summary and error detail are
//     preserved in the message.
//
// Both are non-retryable — repeating the identical invocation reproduces the
// identical refusal — and both are strictly narrower than the original
// behavior, which admitted the claim unconditionally. Blocks that never
// mention content exclusion (nor, alongside runtime evidence, an
// evidenceGatedClaimMarkers phrase) are untouched, so the ordinary
// dependency-block path (docs/stage-contract.md) is unaffected.
//
// #5444 review DA (2026-09-24) found the original "failure" widening
// misclassified genuine cloud-provider access-denied failures (Key Vault, S3,
// IAM) that happened to share the new markers' vocabulary, and could flip an
// ordinary blocked dependency mentioning "access policy" to failure. The fix:
// a "failure" status, and evidenceGatedClaimMarkers on any status, now both
// require ev.denied — actual observed runtime evidence, not vocabulary alone.
// A deliberate #415 non-retryable escalation code is never touched at all,
// regardless of evidence, since overwriting it would silently turn escalation
// into an ordinary retry route.
func reclassifyToolPermissionBlock(result *apiv1.ResultEnvelope, transcript, stderr []byte) {
	if result == nil {
		return
	}
	if result.Status != apiv1.ResultBlocked && result.Status != apiv1.ResultFailure {
		return
	}
	if result.Error != nil && !result.Error.Retryable && nonRetryableEscalationCodes[result.Error.Code] {
		return
	}

	ev := observeToolPermissions(transcript, stderr)
	// evidenceGatedClaimMarkers only complete a claim alongside ev.denied —
	// see its doc comment. claimsContentExclusion (the unconditional marker
	// set) is unaffected.
	claimed := claimsContentExclusion(*result) || (ev.denied && matchesClaimMarkers(*result, evidenceGatedClaimMarkers))
	if !claimed {
		return
	}
	if ev.contentExclusionSignalled {
		return
	}

	authored := authoredCause(*result)

	if ev.denied {
		result.Status = apiv1.ResultFailure
		if result.Outputs == nil {
			result.Outputs = map[string]interface{}{}
		}
		result.Outputs["contentExclusionClaimRejected"] = true
		result.Outputs["toolPermissionDenied"] = true
		result.Error = &apiv1.ErrorInfo{
			Code:      ErrorCodeToolPermissionDenied,
			Retryable: false,
			Message: fmt.Sprintf(
				"the harness observed a tool-permission refusal, not an organization content-exclusion policy; "+
					"grant the tool to this goober or fix the harness invocation. Runtime evidence: %s. "+
					"The agent reported: %s",
				strings.Join(ev.quotes, " | "), authored),
		}
		result.Summary = "tool permission denied (reported by the agent as content exclusion)"
		return
	}

	// No runtime evidence of any tool-permission refusal. Only a "blocked"
	// result may still be reclassified through the unsubstantiated-claim
	// path (#2962's original design); a "failure" already told the operator
	// something went wrong, and without runtime denial evidence there is
	// nothing this guard can prove it should correct (#5444 review DA).
	if result.Status != apiv1.ResultBlocked {
		return
	}
	result.Status = apiv1.ResultFailure
	if result.Outputs == nil {
		result.Outputs = map[string]interface{}{}
	}
	result.Outputs["contentExclusionClaimRejected"] = true
	result.Outputs["toolPermissionDenied"] = false
	result.Error = &apiv1.ErrorInfo{
		Code:      ErrorCodeUnsubstantiatedContentExclusion,
		Retryable: false,
		Message: fmt.Sprintf(
			"the agent classified this run as blocked by organization content exclusion, but the runtime never "+
				"signalled one; content exclusion is a policy fact, not an inference from a failed tool call. "+
				"The agent reported: %s",
			authored),
	}
	result.Summary = "unsubstantiated content-exclusion claim"
}

// authoredCause renders what the model actually said, so a reclassified
// result never loses the original narrative.
func authoredCause(result apiv1.ResultEnvelope) string {
	var parts []string
	if result.Error != nil {
		if result.Error.Code != "" {
			parts = append(parts, result.Error.Code)
		}
		if result.Error.Message != "" {
			parts = append(parts, result.Error.Message)
		}
	}
	if result.Summary != "" {
		parts = append(parts, result.Summary)
	}
	if len(parts) == 0 {
		return "(no detail)"
	}
	return truncateRunes(strings.Join(parts, ": "), 800)
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
