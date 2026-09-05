// Package secretpattern owns the single definition of the secret-shape pattern
// net: the regexps that identify credential-shaped material that was never
// registered with a run's exact-value scrubber.
//
// It is a leaf with no repo-internal dependencies on purpose. The net has two
// consumers that must agree byte for byte but cannot import each other: the
// journal's runtime boundary scrubber (internal/journal), which redacts what a
// stage writes, and the author-time SEC001 check (api/validate), which refuses a
// secret-shaped literal in history-resident stage inputs before a run exists.
// Keeping the patterns here means an author's finding names exactly the shapes
// the runtime would otherwise have had to redact, with no second copy to drift
// (#2931).
//
// Azure DevOps PATs are intentionally not included: their bare 52-character
// base32 shape has no distinguishing prefix, so matching it would create an
// unacceptably broad false-positive net.
package secretpattern

import "regexp"

// Redacted is the placeholder that replaces scrubbed secret material. It is
// stable so digests over scrubbed bytes are reproducible across runners.
const Redacted = "[REDACTED]"

// RedactedToken is the placeholder that replaces ONLY the credential value of a
// match whose surrounding syntax must survive — today, an authorization
// expression's scheme (#3135). Scrubbed diffs, verdicts, and repass context are
// the evidence agentic review gates reason about, so collapsing an
// authorization header to a single marker made correct code and synthetic
// fixtures read as malformed headers. Redacting at the value boundary removes
// the credential while leaving the scheme, quotes, and variable references
// intact. Like Redacted it is stable, so digests over scrubbed bytes stay
// reproducible across runners.
const RedactedToken = "<redacted-token>"

// pattern pairs a secret-shaped pattern with the replacement template applied to
// each match. A template may reference capture groups (`${1}`) so a match is
// redacted at the value boundary only, keeping the structure a reader needs — an
// authorization scheme, a quote, a variable reference — while the credential
// itself is removed.
type pattern struct {
	re          *regexp.Regexp
	replacement string
}

// defaultPatterns matches secret-shaped material that was never registered — a
// defense-in-depth net for provider tokens that reach the journal without going
// through the resolver. Patterns are intentionally specific to keep false
// positives low; the registry is the primary mechanism.
var defaultPatterns = []pattern{
	// GitHub tokens: ghp_, gho_, ghu_, ghs_, ghr_, github_pat_.
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), Redacted},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`), Redacted},
	// AWS access key id.
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), Redacted},
	// Slack tokens.
	{regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), Redacted},
	// Anthropic API keys and GitLab personal access tokens.
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), Redacted},
	{regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`), Redacted},
	// JWTs: require the conventional base64url-encoded JSON header prefix.
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), Redacted},
	// PEM private key blocks.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), Redacted},
	// Bearer/authorization header values with a long opaque token. The scheme
	// is captured and restored: only the value is a credential, and a reviewer
	// judging a diff must still be able to see that the header is well formed.
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/-]{20,}=*`), "${1}" + RedactedToken},
	{regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+/]{16,}=*`), "${1}" + RedactedToken},
}

// Scrubber redacts secret-shaped substrings using a set of regexps.
type Scrubber struct {
	patterns []pattern
}

// NewScrubber returns a scrubber using the default secret patterns.
func NewScrubber() *Scrubber {
	return &Scrubber{patterns: defaultPatterns}
}

// Scrub replaces every pattern match with its placeholder: the whole match for a
// value-shaped pattern, and the value alone (scheme preserved) for an
// authorization expression.
func (s *Scrubber) Scrub(b []byte) []byte {
	out := b
	for _, p := range s.patterns {
		out = p.re.ReplaceAll(out, []byte(p.replacement))
	}
	return out
}
