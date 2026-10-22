package main

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"slices"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
)

// credentialrefresh.go is the daemon side of mid-stage credential refresh
// (Goobers#6120 phase 1; distributed-state-and-coordination.md DS10, §11,
// acceptance item 8).
//
// THREAT MODEL, in one place:
//
//   - WHO GETS A GRANT. Only a deterministic task's attempt: the local runner
//     mints one for a goobers-CLI stage whose delivered credentials state an
//     expiry (executor.appendCredentialGrant), and the credential plane mints
//     one for a pod's stage-start resolve when the pinned definition says the
//     stage is a deterministic task (mintPodStageGrant). Agentic stages and
//     reviewer gates never get one (phase 2), and the refresh route refuses
//     them even if a grant for one were forged into existence.
//   - WHAT IT MINTS. One capability per call, from the grant's own list —
//     the stage's declared capabilities whose delivered value expires — and
//     re-verified against the run's PINNED definition on every call, through
//     the same capability-gated injector the stage-start resolve uses.
//   - HOW LONG. The stage timeout plus a margin, capped at
//     podauth.MaxCredentialGrantTTL.
//   - HOW IT ENDS. It expires; process-local revocation resets on restart or another replica, while startup key rotation invalidates outstanding grants.
//   - HOW FAST. Each grant is rate-limited (grantRefreshBurst, then one per
//     grantRefreshInterval), and every refresh is journaled with the attempt.

// credentialRefreshMarker identifies a mid-stage re-resolve's audit record.
const credentialRefreshMarker = "credentials.refreshed"

// grantRefreshBurst and grantRefreshInterval bound how often one grant may
// mint. A stage refreshes a capability when it nears expiry or after a 401,
// so a handful of calls an hour is the expected rate; the burst leaves room
// for several capabilities expiring together.
func newStageGrantIssuer(endpoint string) (*planes.StageGrantIssuer, error) {
	key, err := podauth.CredentialGrantSigningKey("")
	if err != nil {
		return nil, err
	}
	return planes.NewStageGrantIssuer(key, endpoint)
}

const grantRefreshBurst = 6

// withStageGrants enables mid-stage credential refresh on this daemon's
// credential plane, served at the API reachable at listenAddress, and offers
// the minter to the local runners of the instance at root. A daemon that
// cannot construct the issuer keeps serving with refresh disabled: stages
// then behave exactly as they did before grants existed.
func (s *daemonCredentialService) withStageGrants(root, listenAddress string, tls bool) *daemonCredentialService {
	endpoint, err := planes.LocalCredentialEndpoint(listenAddress, tls)
	if err != nil {
		return s
	}
	keyFile := ""
	if s.config != nil {
		keyFile = s.config.API.PodTokenKeyFile
	}
	key, err := podauth.CredentialGrantSigningKey(keyFile)
	if err != nil {
		log.Printf("warning: credential-refresh grants and child-workflow stage authority are disabled: %v", err)
		return s
	}
	if keyFile == "" {
		log.Printf("warning: api.podTokenKeyFile is not configured; credential-refresh grants will not survive a daemon restart")
	}
	s.grants, err = planes.NewStageGrantIssuer(key, endpoint)
	if err != nil {
		log.Printf("warning: credential-refresh grants and child-workflow stage authority are disabled: %v", err)
		return s
	}
	registerStageGrantMinter(root, s)
	return s
}

// grantKey is the verifier the API authenticator admits grants with; nil when
// refresh is disabled.
func (s *daemonCredentialService) grantKey() *podauth.SignedKey {
	if s == nil || s.grants == nil {
		return nil
	}
	return s.grants.Key()
}

// MintStageGrant implements executor.StageCredentialGrants for the local
// runner: a grant for exactly capabilities of env's attempt, revoked when the
// attempt returns.
//
// The grant names the bare stage, never env.TaskID. The local runner's
// envelope TaskID is the run-scoped "<runId>:<stage>" instance id, while the
// refresh route verifies grant.Stage against the run's PINNED definition by
// stage name, so stamping the composite id refused every local refresh with
// 404 stage_unknown (Goobers#6193). It is the same run qualifier #4119 strips
// from stage-artifact names, so the same helper strips it here. Only this
// run's own prefix is stripped: a TaskID of any other shape is carried
// verbatim and the pinned-definition check still refuses it.
func (s *daemonCredentialService) MintStageGrant(env apiv1.InvocationEnvelope, capabilities []string, ttl time.Duration) (executor.StageCredentialGrant, error) {
	if s.grants == nil {
		return executor.StageCredentialGrant{}, errors.New("credential refresh is not enabled on this daemon")
	}
	return s.grants.MintStageGrant(env.RunID, stageArtifactName(env.RunID, env.TaskID), env.Attempt, capabilities, ttl, s.shared)
}

// mintPodStageGrant mints the grant a pod hands its deterministic stage
// child. Nil — no grant, the pre-grant behavior — when refresh is disabled,
// the stage is not a deterministic task, or none of its credentials expire.
func (s *daemonCredentialService) mintPodStageGrant(request httpapi.CredentialResolveRequest, resolved stageResolution) *httpapi.CredentialGrantDelivery {
	if s.grants == nil || !resolved.profile.deterministic {
		return nil
	}
	expiring := expiringDeclaredCapabilities(resolved)
	if len(expiring) == 0 {
		return nil
	}
	timeout := resolved.profile.timeout
	if timeout <= 0 {
		timeout = time.Duration(request.TimeoutSeconds) * time.Second
	}
	return s.grants.MintPodGrant(request, expiring, executor.CredentialGrantTTL(timeout), s.shared)
}

// expiringDeclaredCapabilities lists the stage's DECLARED capabilities whose
// minted value states an expiry. Implicit keys (the checkout capability) are
// never in a grant: the stage never receives them.
func expiringDeclaredCapabilities(resolved stageResolution) []string {
	var expiring []string
	for _, minted := range resolved.minted {
		if minted.ExpiresAt != nil && slices.Contains(resolved.profile.capabilities, minted.Capability) {
			expiring = append(expiring, minted.Capability)
		}
	}
	return expiring
}

// Refresh implements httpapi.CredentialRefreshService.
func (s *daemonCredentialService) Refresh(ctx context.Context, token string, request httpapi.CredentialRefreshRequest) (httpapi.CredentialResolveResponse, error) {
	grant, err := s.grants.Admit(token, request.Capability)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	resolveRequest := httpapi.CredentialResolveRequest{
		RunID: grant.RunID, Stage: grant.Stage, Capabilities: []string{request.Capability},
	}
	resolved, err := s.resolveStage(ctx, resolveRequest, stageResolveMode{
		marker: credentialRefreshMarker, deterministicOnly: true, attempt: grant.Attempt,
	})
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	return resolved.response(resolveRequest), nil
}

// stageGrantMinters maps an absolute instance root to the minter its local
// runners deliver grants from. The daemon registers itself once its API is
// bound; a process that never registers (goobers run, a worker) mints none.
var stageGrantMinters sync.Map

func registerStageGrantMinter(root string, minter executor.StageCredentialGrants) {
	if absolute, err := filepath.Abs(root); err == nil {
		stageGrantMinters.Store(absolute, minter)
	}
}

// stageGrantMinterFor returns the minter registered for instanceRoot, or nil.
func stageGrantMinterFor(instanceRoot string) executor.StageCredentialGrants {
	absolute, err := filepath.Abs(instanceRoot)
	if err != nil {
		return nil
	}
	minter, ok := stageGrantMinters.Load(absolute)
	if !ok {
		return nil
	}
	return minter.(executor.StageCredentialGrants)
}
