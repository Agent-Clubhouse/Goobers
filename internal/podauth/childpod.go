package podauth

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/httpapi"
)

const childPodTokenPrefix = "goobers-child-pod."
const childPodMACDomain = "child-pod/v1:"

// MintChildPod binds generated custody into the signed identity. Its MAC domain
// prevents removing the marker to obtain an ordinary pod's shared blob access.
func (s *SignedKey) MintChildPod(runID, contractDigest string, ttl time.Duration) (string, error) {
	return s.mintContractPod(runID, contractDigest, ttl, childPodTokenPrefix, childPodMACDomain)
}

func (s *SignedKey) mintContractPod(runID, contractDigest string, ttl time.Duration, prefix, domain string) (string, error) {
	if !blobstore.ValidDigest(contractDigest) {
		return "", ErrMalformedToken
	}
	payload, err := s.custodyPayload(runID, ttl)
	if err != nil {
		return "", err
	}
	payload += "." + base64.RawURLEncoding.EncodeToString([]byte(contractDigest))
	return prefix + payload + "." + s.sign(domain+payload), nil
}

func (s *SignedKey) custodyPayload(runID string, ttl time.Duration) (string, error) {
	ordinary, err := s.Mint(runID, ttl)
	if err != nil {
		return "", err
	}
	payload := strings.TrimPrefix(ordinary, tokenPrefix)
	payload = payload[:strings.LastIndex(payload, ".")]
	return payload, nil
}

func (s *SignedKey) verifyChildPod(token string) (string, string, error) {
	return s.verifyContractPod(token, childPodTokenPrefix, childPodMACDomain)
}
func (s *SignedKey) verifyContractPod(token, prefix, domain string) (string, string, error) {
	payload, err := s.verifyCustodyMAC(token, prefix, domain)
	if err != nil {
		return "", "", err
	}
	idx := strings.LastIndex(payload, ".")
	if idx <= 0 {
		return "", "", ErrMalformedToken
	}
	digest, err := base64.RawURLEncoding.DecodeString(payload[idx+1:])
	if err != nil || !blobstore.ValidDigest(string(digest)) {
		return "", "", ErrMalformedToken
	}
	runID, err := s.parseCustodyPayload(payload[:idx])
	return runID, string(digest), err
}

func (s *SignedKey) verifyCustodyMAC(token, prefix, domain string) (string, error) {
	rest, ok := strings.CutPrefix(token, prefix)
	idx := strings.LastIndex(rest, ".")
	if !ok || idx <= 0 || idx == len(rest)-1 {
		return "", ErrMalformedToken
	}
	payload, mac := rest[:idx], rest[idx+1:]
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(domain+payload))) != 1 {
		return "", ErrUnknownToken
	}
	return payload, nil
}

func (s *SignedKey) parseCustodyPayload(payload string) (string, error) {
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
	verifier, ok := a.verifier.(interface {
		verifyChildPod(string) (string, string, error)
	})
	if !ok {
		return nil, ErrUnknownToken
	}
	runID, digest, err := verifier.verifyChildPod(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{Subject: "run:" + runID, Issuer: httpapi.GeneratedChildPrincipalIssuer, GeneratedChild: &httpapi.GeneratedChildPrincipal{ContractDigest: digest}}, nil
}
