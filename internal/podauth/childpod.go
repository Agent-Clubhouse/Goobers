package podauth

import (
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
	return s.encodeToken(prefix, domain, payload), nil
}

func (s *SignedKey) custodyPayload(runID string, ttl time.Duration) (string, error) {
	ordinary, err := s.Mint(runID, ttl)
	if err != nil {
		return "", err
	}
	payload := strings.TrimPrefix(ordinary, tokenPrefix)
	payload = payload[:strings.LastIndex(payload, ".")]
	if strings.HasPrefix(payload, "kid:") {
		_, payload, _ = strings.Cut(payload, ".")
	}
	return payload, nil
}

func (s *SignedKey) verifyChildPod(token string) (string, string, error) {
	return s.verifyContractPod(token, childPodTokenPrefix, childPodMACDomain)
}
func (s *SignedKey) verifyContractPod(token, prefix, domain string) (string, string, error) {
	payload, err := s.verifyMAC(token, prefix, domain)
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

func (s *SignedKey) parseCustodyPayload(payload string) (string, error) {
	runID, scopes, expires, err := parseSignedPayload(payload)
	if err != nil || len(scopes) != 0 {
		return "", ErrMalformedToken
	}
	if !s.unexpired(expires) {
		return "", ErrUnknownToken
	}
	return runID, nil
}

type childPodVerifier interface {
	verifyChildPod(string) (string, string, error)
}

func (a *Authenticator) authenticateChildPod(token string) (*httpapi.Principal, error) {
	return authenticateWith(a, func(v childPodVerifier) (*httpapi.Principal, error) {
		runID, digest, err := v.verifyChildPod(token)
		if err != nil {
			return nil, err
		}
		return &httpapi.Principal{Subject: "run:" + runID, Issuer: httpapi.GeneratedChildPrincipalIssuer, GeneratedChild: &httpapi.GeneratedChildPrincipal{ContractDigest: digest}}, nil
	})
}
