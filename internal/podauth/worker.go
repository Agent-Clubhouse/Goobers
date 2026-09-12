package podauth

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const workerTokenPrefix = "goobers-worker."

// This non-wire domain cannot be a valid pod payload: its NUL delimiter is
// forbidden by the pod token's base64/decimal grammar. The wire prefix alone
// is insufficient because it can itself be parsed as a base64 pod segment.
const workerMACDomain = "goobers/worker-config-digest/v1\x00"

// MaxWorkerTokenTTL bounds the config-observability worker identity. Workers
// mint a fresh bearer for each digest poll or transition report; this
// credential never enters a stage pod and reaches no run or config content.
const MaxWorkerTokenTTL = 5 * time.Minute

// MintWorkerConfigDigest authenticates the worker itself, without inventing a
// run identity. The separate wire prefix and MAC domain prevent converting this
// narrow config-observability credential into a run-scoped pod token by
// replacing its prefix.
func (s *SignedKey) MintWorkerConfigDigest(workerID string, ttl time.Duration) (string, error) {
	if !validWorkerID(workerID) {
		return "", fmt.Errorf("podauth: a nonempty worker identity of at most 256 bytes without control characters is required")
	}
	if ttl <= 0 || ttl > MaxWorkerTokenTTL {
		return "", fmt.Errorf("podauth: worker token TTL must be positive and at most %s", MaxWorkerTokenTTL)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(workerID)) + "." + strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return workerTokenPrefix + payload + "." + s.sign(workerMACDomain+payload), nil
}

func validWorkerID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func (s *SignedKey) verifyWorkerConfigDigest(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, workerTokenPrefix)
	if !ok || len(rest) > 512 {
		return "", ErrMalformedToken
	}
	segments := strings.Split(rest, ".")
	if len(segments) != 3 {
		return "", ErrMalformedToken
	}
	payload := segments[0] + "." + segments[1]
	if subtle.ConstantTimeCompare([]byte(segments[2]), []byte(s.sign(workerMACDomain+payload))) != 1 {
		return "", ErrUnknownToken
	}
	identity, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil || !validWorkerID(string(identity)) {
		return "", ErrMalformedToken
	}
	exp, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil {
		return "", ErrMalformedToken
	}
	now := s.now()
	if !time.Unix(exp, 0).After(now) || time.Unix(exp, 0).After(now.Add(MaxWorkerTokenTTL)) {
		return "", ErrUnknownToken
	}
	return string(identity), nil
}
