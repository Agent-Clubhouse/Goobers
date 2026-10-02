package podauth

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

const workerSurrenderTokenPrefix = "goobers-worker-surrender-read."

// This non-wire domain cannot be a valid pod payload: its NUL delimiter is
// forbidden by the pod token's base64/decimal grammar. The wire prefix alone
// is insufficient because it can itself be parsed as a base64 pod segment.
const workerSurrenderMACDomain = "goobers/worker-surrender-read/v1\x00"

// MintWorkerSurrender grants a resident worker access only to surrender GET/Seen.
// Its wire prefix and MAC domain are separate from pod and config-digest tokens.
func (s *SignedKey) MintWorkerSurrender(workerID string, ttl time.Duration) (string, error) {
	if !validWorkerID(workerID) {
		return "", fmt.Errorf("podauth: a nonempty worker identity of at most 256 bytes without control characters is required")
	}
	if ttl <= 0 || ttl > MaxWorkerTokenTTL {
		return "", fmt.Errorf("podauth: worker token TTL must be positive and at most %s", MaxWorkerTokenTTL)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(workerID)) + "." + strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return workerSurrenderTokenPrefix + payload + "." + s.sign(workerSurrenderMACDomain+payload), nil
}

func (s *SignedKey) verifyWorkerSurrender(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, workerSurrenderTokenPrefix)
	if !ok || len(rest) > 512 {
		return "", ErrMalformedToken
	}
	segments := strings.Split(rest, ".")
	if len(segments) != 3 {
		return "", ErrMalformedToken
	}
	payload := segments[0] + "." + segments[1]
	if subtle.ConstantTimeCompare([]byte(segments[2]), []byte(s.sign(workerSurrenderMACDomain+payload))) != 1 {
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

func (a *Authenticator) authenticateWorkerSurrender(token string) (*httpapi.Principal, error) {
	verifier, ok := a.verifier.(interface{ verifyWorkerSurrender(string) (string, error) })
	if !ok {
		return nil, ErrUnknownToken
	}
	id, err := verifier.verifyWorkerSurrender(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{Subject: "worker:" + id, Issuer: httpapi.WorkerSurrenderPrincipalIssuer}, nil
}
