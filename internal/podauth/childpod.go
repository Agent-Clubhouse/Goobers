package podauth

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

const childPodTokenPrefix = "goobers-child-pod."
const childPodMACDomain = "child-pod/v1:"

// MintChildPod binds generated custody into the signed identity. Its MAC domain
// prevents removing the marker to obtain an ordinary pod's shared blob access.
func (s *SignedKey) MintChildPod(runID string, ttl time.Duration) (string, error) {
	ordinary, err := s.Mint(runID, ttl)
	if err != nil {
		return "", err
	}
	payload := strings.TrimPrefix(ordinary, tokenPrefix)
	payload = payload[:strings.LastIndex(payload, ".")]
	return childPodTokenPrefix + payload + "." + s.sign(childPodMACDomain+payload), nil
}

func (s *SignedKey) verifyChildPod(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, childPodTokenPrefix)
	idx := strings.LastIndex(rest, ".")
	if !ok || idx <= 0 || idx == len(rest)-1 {
		return "", ErrMalformedToken
	}
	payload, mac := rest[:idx], rest[idx+1:]
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(childPodMACDomain+payload))) != 1 {
		return "", ErrUnknownToken
	}
	runID, scopes, expires, err := parseSignedPayload(payload)
	if err != nil || len(scopes) != 0 {
		return "", ErrMalformedToken
	}
	if !time.Unix(expires, 0).After(s.now()) {
		return "", ErrUnknownToken
	}
	return runID, nil
}

func (a *Authenticator) authenticateChildPod(token string) (*httpapi.Principal, error) {
	verifier, ok := a.verifier.(interface{ verifyChildPod(string) (string, error) })
	if !ok {
		return nil, ErrUnknownToken
	}
	runID, err := verifier.verifyChildPod(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{Subject: "run:" + runID, Issuer: httpapi.PodPrincipalIssuer, GeneratedChild: true}, nil
}
