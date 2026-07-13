package journal

import (
	"bytes"
	"regexp"
	"sort"
	"sync"
)

// Redacted is the placeholder that replaces scrubbed secret material. It is
// stable so digests over scrubbed bytes are reproducible across runners.
const Redacted = "[REDACTED]"

// Scrubber removes secret-shaped material from bytes before they are written to
// (and digested into) the journal. Every event, input snapshot, and artifact
// passes through the run's Scrubber before hitting disk, so raw secrets never
// land at rest (SEC-041, TEL-013). Scrub MUST be pure and deterministic: the
// same input yields the same output, because digests commit to the scrubbed
// bytes and conformance depends on those digests.
type Scrubber interface {
	Scrub(b []byte) []byte
}

// ScrubberFunc adapts a function to Scrubber.
type ScrubberFunc func([]byte) []byte

// Scrub implements Scrubber.
func (f ScrubberFunc) Scrub(b []byte) []byte { return f(b) }

// nopScrubber is the default when no scrubber is configured. It is deliberately
// distinct from "no redaction is required": a run always has a Scrubber, and the
// nop is only used by tests and by callers that have proven their inputs carry
// no secrets.
type nopScrubber struct{}

func (nopScrubber) Scrub(b []byte) []byte { return b }

// RegistryScrubber redacts exact secret values registered at runtime — the
// primary defense, fed every credential the secret resolver issues. Redaction of
// known values is exact and cannot false-negative on a value it has been told
// about. It is safe for concurrent use.
type RegistryScrubber struct {
	mu      sync.RWMutex
	secrets map[string][]byte // digest of secret -> secret bytes
}

// NewRegistryScrubber returns an empty registry scrubber.
func NewRegistryScrubber() *RegistryScrubber {
	return &RegistryScrubber{secrets: make(map[string][]byte)}
}

// Register adds a secret value to redact. Empty and very short values are
// ignored: redacting them would corrupt unrelated content for no security gain
// (a one-character "secret" is not a secret). Keying by digest avoids holding
// duplicate copies and never logs the value.
func (s *RegistryScrubber) Register(secret []byte) {
	if len(secret) < minSecretLen {
		return
	}
	key := Digest(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[key]; !ok {
		cp := make([]byte, len(secret))
		copy(cp, secret)
		s.secrets[key] = cp
	}
}

// Scrub replaces every registered secret value with the Redacted placeholder.
// Longer secrets are replaced first so a secret that contains another registered
// value is fully redacted rather than partially unmasked.
func (s *RegistryScrubber) Scrub(b []byte) []byte {
	s.mu.RLock()
	values := make([][]byte, 0, len(s.secrets))
	for _, v := range s.secrets {
		values = append(values, v)
	}
	s.mu.RUnlock()
	if len(values) == 0 {
		return b
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	out := b
	for _, v := range values {
		out = bytes.ReplaceAll(out, v, []byte(Redacted))
	}
	return out
}

// minSecretLen is the shortest value the registry will redact.
const minSecretLen = 6

// defaultSecretPatterns matches secret-shaped material that was never registered
// — a defense-in-depth net for provider tokens that reach the journal without
// going through the resolver. Patterns are intentionally specific to keep false
// positives low; the registry is the primary mechanism.
var defaultSecretPatterns = []*regexp.Regexp{
	// GitHub tokens: ghp_, gho_, ghu_, ghs_, ghr_, github_pat_.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`),
	// AWS access key id.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// Slack tokens.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// PEM private key blocks.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	// Bearer/authorization header values with a long opaque token.
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
}

// PatternScrubber redacts secret-shaped substrings using a set of regexps.
type PatternScrubber struct {
	patterns []*regexp.Regexp
}

// NewPatternScrubber returns a scrubber using the default secret patterns.
func NewPatternScrubber() *PatternScrubber {
	return &PatternScrubber{patterns: defaultSecretPatterns}
}

// Scrub replaces every pattern match with the Redacted placeholder.
func (s *PatternScrubber) Scrub(b []byte) []byte {
	out := b
	for _, re := range s.patterns {
		out = re.ReplaceAll(out, []byte(Redacted))
	}
	return out
}

// multiScrubber applies its members in order.
type multiScrubber []Scrubber

// Scrub runs each member scrubber in sequence.
func (m multiScrubber) Scrub(b []byte) []byte {
	for _, s := range m {
		b = s.Scrub(b)
	}
	return b
}

// Chain composes scrubbers into one applied left to right. The registry (exact,
// no false positives) should come before the pattern net.
func Chain(scrubbers ...Scrubber) Scrubber {
	switch len(scrubbers) {
	case 0:
		return nopScrubber{}
	case 1:
		return scrubbers[0]
	default:
		return multiScrubber(scrubbers)
	}
}

// DefaultScrubber returns the standard boundary scrubber: a registry (which the
// caller feeds resolver-issued credentials) chained before the pattern net.
func DefaultScrubber() (*RegistryScrubber, Scrubber) {
	reg := NewRegistryScrubber()
	return reg, Chain(reg, NewPatternScrubber())
}
