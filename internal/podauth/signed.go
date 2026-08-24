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
// elapses (DefaultTokenTTL when zero, capped at MaxSignedTokenTTL).
func (s *SignedKey) Mint(runID string, ttl time.Duration) (string, error) {
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
	exp := s.now().Add(ttl).UTC().Unix()
	payload := signedPayload(runID, exp)
	return tokenPrefix + payload + "." + s.sign(payload), nil
}

// verifySigned resolves a presented signed token to its run ID.
func (s *SignedKey) verifySigned(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return "", ErrMalformedToken
	}
	idx := strings.LastIndex(rest, ".")
	if idx <= 0 || idx == len(rest)-1 {
		return "", ErrMalformedToken
	}
	payload, mac := rest[:idx], rest[idx+1:]
	// Constant-time compare: a byte-wise early return here leaks how much of a
	// forged MAC was correct.
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(payload))) != 1 {
		return "", ErrUnknownToken
	}
	runID, exp, err := parseSignedPayload(payload)
	if err != nil {
		return "", err
	}
	if !time.Unix(exp, 0).After(s.now()) {
		return "", ErrUnknownToken
	}
	return runID, nil
}

func (s *SignedKey) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func signedPayload(runID string, exp int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(runID)) + "." + strconv.FormatInt(exp, 10)
}

func parseSignedPayload(payload string) (string, int64, error) {
	encoded, expText, ok := strings.Cut(payload, ".")
	if !ok {
		return "", 0, ErrMalformedToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", 0, ErrMalformedToken
	}
	exp, err := strconv.ParseInt(expText, 10, 64)
	if err != nil {
		return "", 0, ErrMalformedToken
	}
	return string(raw), exp, nil
}
