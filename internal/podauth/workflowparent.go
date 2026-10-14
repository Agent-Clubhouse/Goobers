package podauth

import (
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

// WorkflowParentPodPrefix identifies the separate signed parent token domain.
// The prefix alone never authenticates a caller.
const WorkflowParentPodPrefix = "goobers-workflow-parent-pod."
const workflowParentPodMACDomain = "workflow-parent-pod/v1:"

// MintWorkflowParentPod signs exact parent custody in its own authority domain.
func (s *SignedKey) MintWorkflowParentPod(runID, contractDigest string, ttl time.Duration) (string, error) {
	return s.mintContractPod(runID, contractDigest, ttl, WorkflowParentPodPrefix, workflowParentPodMACDomain)
}
func (s *SignedKey) verifyWorkflowParentPod(token string) (string, string, error) {
	return s.verifyContractPod(token, WorkflowParentPodPrefix, workflowParentPodMACDomain)
}

type workflowParentVerifier interface {
	verifyWorkflowParentPod(string) (string, string, error)
}

func (a *Authenticator) authenticateWorkflowParentPod(token string) (*httpapi.Principal, error) {
	return authenticateWith(a, func(v workflowParentVerifier) (*httpapi.Principal, error) {
		runID, digest, err := v.verifyWorkflowParentPod(token)
		if err != nil {
			return nil, err
		}
		return &httpapi.Principal{Subject: "run:" + runID, Issuer: httpapi.WorkflowParentPrincipalIssuer, WorkflowParent: &httpapi.WorkflowParentPrincipal{ContractDigest: digest}}, nil
	})
}
