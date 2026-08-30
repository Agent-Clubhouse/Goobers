package journal

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"sync"
)

// Redacted is the placeholder that replaces scrubbed secret material. It is
// stable so digests over scrubbed bytes are reproducible across runners.
const Redacted = "[REDACTED]"

// RedactedToken replaces only the credential VALUE inside an authorization
// expression, leaving the scheme and the surrounding syntax intact (#3135).
// Swallowing the whole scheme-plus-value span corrupted code evidence: a review
// gate reading a scrubbed diff saw the authorization scheme itself replaced and
// reported the header as malformed, contradicting the authoritative raw diff.
// It is deliberately distinct from Redacted so a reader (human or agent) can
// tell that a credential value was removed rather than a whole span of secret
// material.
const RedactedToken = "<redacted-token>"

// Scrubber removes secret-shaped material from bytes before they are written to
// (and digested into) the journal. Every event, input snapshot, and artifact
// passes through the run's Scrubber before hitting disk, so raw secrets never
// land at rest (SEC-041, TEL-013). Scrub MUST be pure and deterministic: the
// same input yields the same output, because digests commit to the scrubbed
// bytes and conformance depends on those digests.
type Scrubber interface {
	Scrub(b []byte) []byte
}

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

// Scrub replaces every registered secret value with the Redacted placeholder,
// in both its raw form AND its JSON-string-escaped form. The journal marshals an
// event to JSON before scrubbing the marshaled bytes (see appendEvent), so a
// secret containing any JSON-escaped byte — a quote, backslash, control char, or
// the HTML-escaped <, >, & — reaches the scrubber in its escaped form. Matching
// only the raw bytes would let that escaped form land at rest (SEC-041, #114), so
// the escaped encodings are redacted too.
//
// Longer targets are replaced first (with a byte-order tiebreak for full
// determinism, since digests commit to the scrubbed output) so a value that
// contains another registered value — or whose escaped form contains another
// target — is fully redacted rather than partially unmasked.
func (s *RegistryScrubber) Scrub(b []byte) []byte {
	s.mu.RLock()
	targets := make([][]byte, 0, len(s.secrets)*2)
	for _, v := range s.secrets {
		targets = append(targets, v)
		targets = append(targets, jsonEscapedForms(v)...)
	}
	s.mu.RUnlock()
	if len(targets) == 0 {
		return b
	}
	sort.Slice(targets, func(i, j int) bool {
		if len(targets[i]) != len(targets[j]) {
			return len(targets[i]) > len(targets[j])
		}
		return bytes.Compare(targets[i], targets[j]) < 0
	})
	out := b
	for _, t := range targets {
		out = bytes.ReplaceAll(out, t, []byte(Redacted))
	}
	return out
}

// jsonEscapedForms returns the JSON-string encodings of v (without the
// surrounding quotes) that differ from v's raw bytes — the exact byte sequences
// v becomes as a field value in a marshaled event. It returns both the
// HTML-escaping form (Go's json.Marshal default, which the journal's appendEvent
// uses) and the non-HTML-escaping form, so a secret is redacted whichever way an
// encoder was configured. Marshaling a Go string cannot fail, so error paths
// simply contribute no form.
func jsonEscapedForms(v []byte) [][]byte {
	var forms [][]byte
	add := func(inner []byte) {
		if len(inner) == 0 || bytes.Equal(inner, v) {
			return
		}
		for _, existing := range forms {
			if bytes.Equal(existing, inner) {
				return
			}
		}
		forms = append(forms, inner)
	}

	// HTML-escaping encoder (matches the journal's json.Marshal).
	if enc, err := json.Marshal(string(v)); err == nil && len(enc) >= 2 {
		add(enc[1 : len(enc)-1])
	}
	// Non-HTML-escaping encoder (a caller may disable HTML escaping).
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(string(v)); err == nil {
		enc := bytes.TrimRight(buf.Bytes(), "\n") // Encoder appends a trailing newline
		if len(enc) >= 2 {
			add(enc[1 : len(enc)-1])
		}
	}
	return forms
}

// minSecretLen is the shortest value the registry will redact.
const minSecretLen = 6

// secretPattern is one entry in the pattern net: a regexp and the replacement
// template applied to each match. A template may reference the match's named
// groups, which is how an authorization expression keeps its scheme and spacing
// while only its value is removed (#3135).
type secretPattern struct {
	re   *regexp.Regexp
	repl string
}

// defaultSecretPatterns matches secret-shaped material that was never registered
// — a defense-in-depth net for provider tokens that reach the journal without
// going through the resolver. Patterns are intentionally specific to keep false
// positives low; the registry is the primary mechanism.
var defaultSecretPatterns = []secretPattern{
	// GitHub tokens: ghp_, gho_, ghu_, ghs_, ghr_, github_pat_.
	{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), repl: Redacted},
	{re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`), repl: Redacted},
	// AWS access key id.
	{re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`), repl: Redacted},
	// Slack tokens.
	{re: regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), repl: Redacted},
	// PEM private key blocks.
	{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), repl: Redacted},
	// Authorization header values with a long opaque token. Only the value is
	// replaced — the scheme and the separating spacing survive, so a diff, a
	// verdict rationale, or a repass context handed to an agentic gate still
	// reads as the code it came from (#3135). The separator is horizontal
	// whitespace only: `\s` would span a newline and let the net swallow the
	// first token-shaped word of the following line.
	{re: regexp.MustCompile(`(?i)(?P<scheme>bearer)(?P<sep>[ \t]+)[A-Za-z0-9._~+/-]{20,}=*`), repl: "${scheme}${sep}" + RedactedToken},
	{re: regexp.MustCompile(`(?i)(?P<scheme>basic)(?P<sep>[ \t]+)[A-Za-z0-9+/]{16,}=*`), repl: "${scheme}${sep}" + RedactedToken},
}

// PatternScrubber redacts secret-shaped substrings using a set of regexps.
type PatternScrubber struct {
	patterns []secretPattern
}

// NewPatternScrubber returns a scrubber using the default secret patterns.
func NewPatternScrubber() *PatternScrubber {
	return &PatternScrubber{patterns: defaultSecretPatterns}
}

// Scrub replaces every pattern match with its replacement: the whole match for a
// provider-token shape, or the credential value alone for an authorization
// expression, whose scheme and spacing are preserved so code evidence stays
// readable (#3135).
func (s *PatternScrubber) Scrub(b []byte) []byte {
	out := b
	for _, p := range s.patterns {
		out = p.re.ReplaceAll(out, []byte(p.repl))
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
