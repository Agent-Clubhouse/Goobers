package telemetry

import (
	"regexp"

	"github.com/goobers/goobers/internal/journal"
)

// RedactedPlaceholder replaces a secret-shaped match in exported telemetry. It
// is journal.Redacted so a redaction reads identically whether it landed in a
// run's events.jsonl or in an exported span — one placeholder across the system
// (#117).
const RedactedPlaceholder = journal.Redacted

// providerNet is the single, shared provider-token pattern net: journal's
// canonical net (internal/journal, #8/#114), reused here rather than maintained
// as a second, drifting copy. Before #117 this package kept its own list that
// had already diverged from journal's (looser GitHub/PAT thresholds, no Slack) —
// a secret shape caught in the journal could slip through a span, or vice versa.
// Sourcing the net from journal keeps the two in lockstep by construction.
var providerNet = journal.NewPatternScrubber()

// telemetryOnlyPatterns are extra, deliberately aggressive nets applied ONLY to
// telemetry output. Span attributes, status messages, and rollup free-text are
// ephemeral and never conformance-normative, so over-redaction here is harmless
// — unlike journal's at-rest content, where a false positive would corrupt an
// artifact and shift its digest. That risk asymmetry is exactly why this net is
// NOT folded into journal's canonical list: it is telemetry-scoped by design,
// not drift. (Folding a key=value net into the at-rest scrubber is a separate,
// conformance-reviewed decision — see #117's Piece B follow-up.)
var telemetryOnlyPatterns = []*regexp.Regexp{
	// key/token/secret/password=value or "value" assignments (JSON, env, query strings).
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)["']?\s*[:=]\s*["']?[A-Za-z0-9._\-/+=]{8,}`),
}

// redactString returns s with every secret-shaped substring replaced by
// RedactedPlaceholder: first the shared provider-token net, then telemetry's
// own ephemeral-only nets.
func redactString(s string) string {
	out := providerNet.Scrub([]byte(s))
	for _, re := range telemetryOnlyPatterns {
		out = re.ReplaceAll(out, []byte(RedactedPlaceholder))
	}
	return string(out)
}

// Redact returns s with any secret-shaped substring replaced. Exported so other
// local (never-at-rest) consumers — notably the rollup ingester — apply the
// identical net as a second, independent redaction pass over free-text journal
// fields (defense in depth alongside #8's journal scrubber and #14's credential
// registry, TEL-013/SEC-041), without duplicating the pattern list.
func Redact(s string) string { return redactString(s) }
