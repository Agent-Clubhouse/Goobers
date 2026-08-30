package podauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxSignedTokenTTL bounds a signed token's life. Revocation is unavailable in
// this mode, so the ceiling is the only containment a leaked token has.
const MaxSignedTokenTTL = 8 * time.Hour

// MinSignedKeyBytes is the shortest accepted signing key. Short keys are
// refused at construction rather than silently accepted: a weak key here is
// indistinguishable from a strong one until someone forges a token.
const MinSignedKeyBytes = 32

// ErrMalformedToken reports a pod-prefixed bearer that is not a well-formed
// signed token. Kept distinct from ErrUnknownToken so a misconfiguration
// (wrong key, truncated file) is not reported as an expiry.
var ErrMalformedToken = errors.New("podauth: malformed signed pod token")

// SignedKey mints and verifies stateless per-run tokens from a shared secret.
// Safe for concurrent use: it holds no mutable state.
//
// WHY THIS EXISTS (Goobers#3701): the in-memory Registry is daemon-local, which
// is sound when the dispatcher shares the daemon's process. Mode-3 splits them:
// `goobers up` and `goobers worker --dispatch-namespace` are separate processes
// (separate Deployments in-cluster), so a token minted in the worker's memory is
// unverifiable by the daemon that receives the surrender.
//
// A shared signing key makes minting and verification independent of shared
// state: the worker signs, the daemon verifies, neither needs the other's memory
// and no bootstrap authentication problem is introduced (a token-issuing
// endpoint would itself need to authenticate the caller).
//
// THE TRADE, STATED: signed tokens cannot be revoked before they expire, so
// Revoke is a no-op in this mode and TTL is the only bound. That is why the
// default TTL is short and why Mint refuses an over-long one.
type SignedKey struct {
	key []byte
	now func() time.Time
}

// NewSignedKey constructs a signer from raw key material.
func NewSignedKey(key []byte) (*SignedKey, error) {
	if len(key) < MinSignedKeyBytes {
		return nil, fmt.Errorf("podauth: signing key must be at least %d bytes, got %d", MinSignedKeyBytes, len(key))
	}
	dup := make([]byte, len(key))
	copy(dup, key)
	return &SignedKey{key: dup, now: time.Now}, nil
}

// WithClock overrides the time source for deterministic tests.
func (s *SignedKey) WithClock(now func() time.Time) *SignedKey {
	s.now = now
	return s
}

// Mint issues a signed bearer authenticating runID's stage pods until ttl
// elapses (DefaultTokenTTL when zero, capped at MaxSignedTokenTTL). The token
// is UNSCOPED — every pod plane — which is what the pod's own runtime needs;
// MintScoped is the least-privilege form handed to a stage subprocess.
func (s *SignedKey) Mint(runID string, ttl time.Duration) (string, error) {
	return s.MintScoped(runID, ttl)
}

// MintScoped issues a signed bearer confined to scopes (see KnownScopes). The
// scopes are inside the signed payload, so a holder cannot widen them: editing
// the segment invalidates the MAC.
func (s *SignedKey) MintScoped(runID string, ttl time.Duration, scopes ...string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", errors.New("podauth: run ID is required")
	}
	if strings.Contains(runID, ".") {
		// The token is dot-delimited; a run ID carrying a dot would make the
		// encoding ambiguous. Refuse rather than mint something that verifies
		// as a different run.
		return "", fmt.Errorf("podauth: run ID %q contains a dot, which the signed token encoding reserves as a delimiter", runID)
	}
	if ttl < 0 {
		return "", fmt.Errorf("podauth: token TTL must not be negative, got %s", ttl)
	}
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if ttl > MaxSignedTokenTTL {
		return "", fmt.Errorf("podauth: signed token TTL %s exceeds the %s ceiling; signed tokens cannot be revoked", ttl, MaxSignedTokenTTL)
	}
	checked, err := checkScopes(scopes)
	if err != nil {
		return "", err
	}
	exp := s.now().Add(ttl).UTC().Unix()
	payload := signedPayload(runID, checked, exp)
	return tokenPrefix + payload + "." + s.sign(payload), nil
}

// verifySigned resolves a presented signed token to its run ID and scopes.
func (s *SignedKey) verifySigned(token string) (string, []string, error) {
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return "", nil, ErrMalformedToken
	}
	idx := strings.LastIndex(rest, ".")
	if idx <= 0 || idx == len(rest)-1 {
		return "", nil, ErrMalformedToken
	}
	payload, mac := rest[:idx], rest[idx+1:]
	// Constant-time compare: a byte-wise early return here leaks how much of a
	// forged MAC was correct.
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(payload))) != 1 {
		return "", nil, ErrUnknownToken
	}
	runID, scopes, exp, err := parseSignedPayload(payload)
	if err != nil {
		return "", nil, err
	}
	if !time.Unix(exp, 0).After(s.now()) {
		return "", nil, ErrUnknownToken
	}
	return runID, scopes, nil
}

func (s *SignedKey) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signedPayload encodes the token's claims. Two shapes, and the two-segment
// one is load-bearing rather than legacy tidiness: an UNSCOPED token keeps the
// exact `b64(runID).exp` encoding it had before scopes existed, so a peer
// process still running the previous build verifies a pod token minted by this
// one. A scoped token adds a third segment; an old verifier refuses it, which
// is the correct direction to fail — an old daemon does not know how to
// confine the bearer, so it must not accept it at all.
func signedPayload(runID string, scopes []string, exp int64) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(runID))
	if len(scopes) == 0 {
		return encoded + "." + strconv.FormatInt(exp, 10)
	}
	return encoded + "." +
		base64.RawURLEncoding.EncodeToString([]byte(strings.Join(scopes, ","))) + "." +
		strconv.FormatInt(exp, 10)
}

func parseSignedPayload(payload string) (string, []string, int64, error) {
	segments := strings.Split(payload, ".")
	if len(segments) != 2 && len(segments) != 3 {
		return "", nil, 0, ErrMalformedToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return "", nil, 0, ErrMalformedToken
	}
	var scopes []string
	if len(segments) == 3 {
		rawScopes, serr := base64.RawURLEncoding.DecodeString(segments[1])
		if serr != nil {
			return "", nil, 0, ErrMalformedToken
		}
		scopes, serr = checkScopes(strings.Split(string(rawScopes), ","))
		if serr != nil {
			// A signed token naming a scope this build does not know is
			// refused, not silently narrowed to the ones it does: the
			// alternative admits a bearer under an authority nobody in this
			// process can reason about.
			return "", nil, 0, ErrMalformedToken
		}
		if len(scopes) == 0 {
			// An explicitly empty scope segment would verify as the UNSCOPED
			// pod token — a widening, from a segment the signer bothered to
			// emit. Refuse it.
			return "", nil, 0, ErrMalformedToken
		}
	}
	exp, err := strconv.ParseInt(segments[len(segments)-1], 10, 64)
	if err != nil {
		return "", nil, 0, ErrMalformedToken
	}
	return string(raw), scopes, exp, nil
}
