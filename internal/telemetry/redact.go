package telemetry

import "regexp"

// RedactedPlaceholder replaces a secret-shaped match in exported telemetry.
const RedactedPlaceholder = "***REDACTED***"

// secretPatterns catches secret-shaped values so they never land at rest in a
// span or the rollup, independent of whatever upstream redaction already ran
// (defense in depth alongside the journal's own scrubber and the credential
// seam's registry, TEL-013/SEC-041). Deliberately conservative and pattern-only
// (no registry here — this package has no visibility into resolver-issued
// credentials); it is a second net, not the primary one.
var secretPatterns = []*regexp.Regexp{
	// GitHub tokens: ghp_/gho_/ghu_/ghs_/ghr_ + github_pat_...
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// Generic bearer tokens.
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{10,}`),
	// AWS access key ids.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// PEM private key blocks.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	// key/token/secret/password=value or "value" assignments (JSON, env, query strings).
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)["']?\s*[:=]\s*["']?[A-Za-z0-9._\-/+=]{8,}`),
}

// redactString returns s with any secret-shaped substring replaced by
// RedactedPlaceholder.
func redactString(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, RedactedPlaceholder)
	}
	return s
}

// Redact returns s with any secret-shaped substring replaced. Exported so
// other local (never-at-rest) consumers — notably the rollup ingester — apply
// the identical pattern net as a second, independent redaction pass over
// free-text journal fields (defense in depth alongside #8's journal scrubber
// and #14's credential registry, TEL-013/SEC-041), without duplicating the
// pattern list.
func Redact(s string) string { return redactString(s) }
