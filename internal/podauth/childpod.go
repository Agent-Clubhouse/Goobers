package podauth

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

const childPodTokenPrefix = "goobers-child-pod."
const childPodMACDomain = "child-pod/v1:"
const workflowParentPodPrefix = "goobers-workflow-parent-pod."
const workflowParentPodMACDomain = "workflow-parent-pod/v1:"

// MintChildPod binds generated custody into the signed identity. Its MAC domain
// prevents removing the marker to obtain an ordinary pod's shared blob access.
func (s *SignedKey) MintChildPod(runID string, ttl time.Duration) (string, error) {
	return s.mintCustodyPod(runID, ttl, childPodTokenPrefix, childPodMACDomain)
}

// MintWorkflowParentPod binds contained parent execution to its own authority
// domain, distinct from generated-child lineage and ordinary shared custody.
func (s *SignedKey) MintWorkflowParentPod(runID string, ttl time.Duration) (string, error) {
	return s.mintCustodyPod(runID, ttl, workflowParentPodPrefix, workflowParentPodMACDomain)
}

func (s *SignedKey) mintCustodyPod(runID string, ttl time.Duration, prefix, domain string) (string, error) {
	ordinary, err := s.Mint(runID, ttl)
	if err != nil {
		return "", err
	}
	payload := strings.TrimPrefix(ordinary, tokenPrefix)
	payload = payload[:strings.LastIndex(payload, ".")]
	return prefix + payload + "." + s.sign(domain+payload), nil
}

func (s *SignedKey) verifyChildPod(token string) (string, error) {
	return s.verifyCustodyPod(token, childPodTokenPrefix, childPodMACDomain)
}

func (s *SignedKey) verifyWorkflowParentPod(token string) (string, error) {
	return s.verifyCustodyPod(token, workflowParentPodPrefix, workflowParentPodMACDomain)
}

func (s *SignedKey) verifyCustodyPod(token, prefix, domain string) (string, error) {
	rest, ok := strings.CutPrefix(token, prefix)
	idx := strings.LastIndex(rest, ".")
	if !ok || idx <= 0 || idx == len(rest)-1 {
		return "", ErrMalformedToken
	}
	payload, mac := rest[:idx], rest[idx+1:]
	if subtle.ConstantTimeCompare([]byte(mac), []byte(s.sign(domain+payload))) != 1 {
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

func (a *Authenticator) authenticateWorkflowParentPod(token string) (*httpapi.Principal, error) {
	verifier, ok := a.verifier.(interface{ verifyWorkflowParentPod(string) (string, error) })
	if !ok {
		return nil, ErrUnknownToken
	}
	runID, err := verifier.verifyWorkflowParentPod(token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Principal{Subject: "run:" + runID, Issuer: httpapi.PodPrincipalIssuer, WorkflowParent: true}, nil
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
