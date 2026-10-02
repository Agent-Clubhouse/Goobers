package journal

import (
	"bytes"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/secretpattern"
)

// Redacted is the placeholder that replaces scrubbed secret material. It is
// stable so digests over scrubbed bytes are reproducible across runners.
const Redacted = secretpattern.Redacted

// RedactedToken is the placeholder that replaces ONLY the credential value of a
// match whose surrounding syntax must survive — today, an authorization
// expression's scheme (#3135). Scrubbed diffs, verdicts, and repass context are
// the evidence agentic review gates reason about, so collapsing
// `Authorization: Bearer <value>` to a single marker made correct code and
// synthetic fixtures read as malformed headers. Redacting at the value boundary
// removes the credential while leaving the scheme, quotes, and variable
// references intact. Like Redacted it is stable, so digests over scrubbed bytes
// stay reproducible across runners.
const RedactedToken = secretpattern.RedactedToken

// MetricRedactionsTotal counts scrub operations that removed secret material.
// Its layer attribute is one of RedactionLayerRegistry or
// RedactionLayerPattern, so operators can distinguish exact-value protection
// from the pattern backstop.
const MetricRedactionsTotal = "goobers.journal.redactions_total"

// RedactionLayer identifies the scrubber layer that removed secret material.
type RedactionLayer string

const (
	// RedactionLayerRegistry identifies exact-value redaction from the secret registry.
	RedactionLayerRegistry RedactionLayer = "registry"
	// RedactionLayerPattern identifies heuristic redaction by the pattern backstop.
	RedactionLayerPattern RedactionLayer = "pattern"
)

// RedactionObserver receives one notification for each Scrub call whose output
// differs from its input. Implementations must be safe for concurrent use.
// Observers are attached per scrubber instance rather than through global OTel
// state, so one process or test cannot redirect another's measurements.
type RedactionObserver interface {
	Redaction(RedactionLayer)
}

type redactionObservation struct {
	mu       sync.RWMutex
	observer RedactionObserver
}

func (o *redactionObservation) set(observer RedactionObserver) {
	o.mu.Lock()
	o.observer = observer
	o.mu.Unlock()
}

func (o *redactionObservation) notify(layer RedactionLayer) {
	o.mu.RLock()
	observer := o.observer
	o.mu.RUnlock()
	if observer != nil {
		observer.Redaction(layer)
	}
}

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
//
// Lifecycle (#2656). A value registered with Register is redacted for the
// scrubber's lifetime. A value registered with RegisterUntil carries the expiry
// its issuer stated, and is retired only RegistryRetirementGrace AFTER that
// expiry: the grace covers output a stage captured while the credential was live
// and flushes later (a buffered artifact, a delayed span batch).
//
// An expiry is a fact about the credential, not about the caller: a minted
// token is dead after the expiry its issuer stated, however many callers
// re-register the same value. So the latest stated expiry wins, a registration
// stating an earlier one never shortens it, and a plain Register (a caller that
// re-resolved the value without learning its expiry) neither pins nor shortens
// a value whose expiry is already known. A value no caller ever stated an
// expiry for is redacted for the scrubber's lifetime. With hourly-rotating
// credentials the registry therefore holds at most about (TTL + grace) / rotation-interval
// rotating entries, plus the permanent ones, instead of growing for the daemon's
// lifetime.
//
// The sorted target list (raw plus JSON-escaped forms, longest first) is built
// once per registry change and shared by every Scrub until the next
// registration or retirement, so steady-state scrubs do no allocation or sort.
type RegistryScrubber struct {
	mu      sync.RWMutex
	secrets map[string]*registryEntry // digest of secret -> entry
	// targets is the sorted redaction list; it is rebuilt (never mutated in
	// place) whenever stale is set, so a Scrub may use a snapshot outside mu.
	targets [][]byte
	stale   bool
	// nextRetire is the earliest retirement deadline among expiring entries;
	// zero when no entry expires.
	nextRetire time.Time
	grace      time.Duration
	now        func() time.Time
	observed   redactionObservation
}

// registryEntry is one registered value with its precomputed escaped forms.
type registryEntry struct {
	forms [][]byte // raw value first, then its JSON-escaped encodings
	// expiresAt is the latest issuer-stated expiry; zero means none was
	// stated and the value is never retired.
	expiresAt time.Time
}

// RegistryRetirementGrace is how long after its stated expiry a RegisterUntil
// value keeps being redacted. It is deliberately far longer than any credential
// TTL or stage flush delay: retiring a value early would let it reach the
// journal unredacted, while keeping it a day longer costs one more comparison
// per scrub.
const RegistryRetirementGrace = 24 * time.Hour

// ExpiringRegistrar is implemented by registrars that can retire a secret after
// its issuer-stated expiry. Callers holding a plain Register-only registrar use
// RegisterSecretUntil, which falls back to permanent registration.
type ExpiringRegistrar interface {
	RegisterUntil(secret []byte, expiresAt time.Time)
}

// RegisterSecretUntil registers secret with r, carrying expiresAt when r
// supports expiry and registering it permanently otherwise. A zero expiresAt is
// a permanent registration.
func RegisterSecretUntil(r interface{ Register([]byte) }, secret []byte, expiresAt time.Time) {
	if e, ok := r.(ExpiringRegistrar); ok {
		e.RegisterUntil(secret, expiresAt)
		return
	}
	r.Register(secret)
}

// NewRegistryScrubber returns an empty registry scrubber.
func NewRegistryScrubber() *RegistryScrubber {
	return &RegistryScrubber{
		secrets: make(map[string]*registryEntry),
		grace:   RegistryRetirementGrace,
		now:     time.Now,
	}
}

// Register adds a secret value to redact for the scrubber's lifetime, unless
// another registration stated its expiry (see RegistryScrubber). Empty and
// very short values are ignored: redacting them would corrupt unrelated content
// for no security gain (a one-character "secret" is not a secret). Keying by
// digest avoids holding duplicate copies and never logs the value.
func (s *RegistryScrubber) Register(secret []byte) {
	s.RegisterUntil(secret, time.Time{})
}

// RegisterUntil adds a secret value to redact until RegistryRetirementGrace
// after expiresAt. A zero expiresAt states no expiry (Register). Re-registering
// a value keeps the latest stated expiry.
func (s *RegistryScrubber) RegisterUntil(secret []byte, expiresAt time.Time) {
	if len(secret) < minSecretLen {
		return
	}
	key := Digest(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.secrets[key]; ok {
		if !expiresAt.IsZero() && (e.expiresAt.IsZero() || expiresAt.After(e.expiresAt)) {
			e.expiresAt = expiresAt
			s.recomputeNextRetireLocked()
		}
	} else {
		cp := make([]byte, len(secret))
		copy(cp, secret)
		s.secrets[key] = &registryEntry{forms: append([][]byte{cp}, jsonEscapedForms(cp)...), expiresAt: expiresAt}
		s.stale = true
		s.noteDeadlineLocked(expiresAt)
	}
	s.pruneLocked(s.now())
}

// noteDeadlineLocked folds one entry's retirement deadline into nextRetire.
func (s *RegistryScrubber) noteDeadlineLocked(expiresAt time.Time) {
	if expiresAt.IsZero() {
		return
	}
	deadline := expiresAt.Add(s.grace)
	if s.nextRetire.IsZero() || deadline.Before(s.nextRetire) {
		s.nextRetire = deadline
	}
}

func (s *RegistryScrubber) recomputeNextRetireLocked() {
	s.nextRetire = time.Time{}
	for _, e := range s.secrets {
		s.noteDeadlineLocked(e.expiresAt)
	}
}

// pruneLocked retires every entry whose deadline has passed. It is a no-op
// until the earliest deadline is reached, so registrations stay O(1) between
// retirements.
func (s *RegistryScrubber) pruneLocked(now time.Time) {
	if s.nextRetire.IsZero() || now.Before(s.nextRetire) {
		return
	}
	for key, e := range s.secrets {
		if !e.expiresAt.IsZero() && !now.Before(e.expiresAt.Add(s.grace)) {
			delete(s.secrets, key)
			s.stale = true
		}
	}
	s.recomputeNextRetireLocked()
}

// redactionTargets returns the current sorted target list, retiring expired
// entries and rebuilding the cache only when the registry changed or a
// retirement deadline passed. The returned slice is never mutated afterwards.
func (s *RegistryScrubber) redactionTargets() [][]byte {
	now := s.now()
	s.mu.RLock()
	targets := s.targets
	fresh := !s.stale && (s.nextRetire.IsZero() || now.Before(s.nextRetire))
	s.mu.RUnlock()
	if fresh {
		return targets
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if s.stale {
		s.targets = sortedTargets(s.secrets)
		s.stale = false
	}
	return s.targets
}

// sortedTargets flattens every entry's forms, longest first with a byte-order
// tiebreak so the replacement order (and so the scrubbed bytes) is
// deterministic.
func sortedTargets(secrets map[string]*registryEntry) [][]byte {
	targets := make([][]byte, 0, len(secrets)*2)
	for _, e := range secrets {
		targets = append(targets, e.forms...)
	}
	sort.Slice(targets, func(i, j int) bool {
		if len(targets[i]) != len(targets[j]) {
			return len(targets[i]) > len(targets[j])
		}
		return bytes.Compare(targets[i], targets[j]) < 0
	})
	return targets
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
	targets := s.redactionTargets()
	if len(targets) == 0 {
		return b
	}
	out := b
	for _, t := range targets {
		out = bytes.ReplaceAll(out, t, []byte(Redacted))
	}
	if !bytes.Equal(out, b) {
		s.observed.notify(RedactionLayerRegistry)
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

// PatternScrubber redacts secret-shaped substrings using a set of regexps. The
// patterns themselves live in internal/secretpattern so the author-time check
// that refuses secret-shaped stage inputs can apply the identical net without
// importing this package (#2931).
type PatternScrubber struct {
	scrubber *secretpattern.Scrubber
	observed redactionObservation
}

// Scrub applies the secret-pattern net.
func (s *PatternScrubber) Scrub(b []byte) []byte {
	out := s.scrubber.Scrub(b)
	if !bytes.Equal(out, b) {
		s.observed.notify(RedactionLayerPattern)
	}
	return out
}

// SafePrefix delegates the streaming boundary calculation to the shared
// pattern implementation, preserving PatternScrubber's checkpoint contract.
func (s *PatternScrubber) SafePrefix(input []byte) int {
	return s.scrubber.SafePrefix(input)
}

// NewPatternScrubber returns a scrubber using the default secret patterns.
func NewPatternScrubber() *PatternScrubber {
	return &PatternScrubber{scrubber: secretpattern.NewScrubber()}
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

type redactionObservable interface {
	setRedactionObserver(RedactionObserver)
}

func (s *RegistryScrubber) setRedactionObserver(observer RedactionObserver) {
	s.observed.set(observer)
}

func (s *PatternScrubber) setRedactionObserver(observer RedactionObserver) {
	s.observed.set(observer)
}

func (m multiScrubber) setRedactionObserver(observer RedactionObserver) {
	for _, scrubber := range m {
		if observable, ok := scrubber.(redactionObservable); ok {
			observable.setRedactionObserver(observer)
		}
	}
}

// ObserveRedactions attaches observer to every observable layer in scrubber.
// It is intentionally separate from Scrubber: custom scrubbers remain valid,
// while standard registry/pattern chains can publish layer-specific metrics.
func ObserveRedactions(scrubber Scrubber, observer RedactionObserver) {
	if observable, ok := scrubber.(redactionObservable); ok {
		observable.setRedactionObserver(observer)
	}
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
