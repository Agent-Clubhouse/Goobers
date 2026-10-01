package executor

import (
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

// Mid-stage credential refresh (Goobers#6120, phase 1). A deterministic
// goobers-CLI stage whose delivered credentials state an expiry also receives
// a stage credential-refresh grant and the daemon endpoint that honors it. The
// stage's refreshing credential sources present the grant to re-resolve one
// capability when a value nears its expiry or the provider answers 401,
// instead of holding the frozen value for the whole stage (DS10, §11,
// acceptance item 8).
//
// Agentic stages never receive either variable in phase 1: the harness holds
// its tokens for the whole session and needs a credential-helper shim and its
// own security review first. TODO(Goobers#6120 phase 2).
const (
	// CredentialEndpointEnvVar is the daemon API root the grant is presented
	// to (the loopback API for a local stage, the daemon API for a pod).
	CredentialEndpointEnvVar = "GOOBERS_CREDENTIAL_ENDPOINT"
	// CredentialGrantEnvVar carries the grant itself: a secret, registered
	// with the stage's scrubbers exactly like a GOOBERS_CRED_* value.
	CredentialGrantEnvVar = "GOOBERS_CREDENTIAL_GRANT"
)

// credentialGrantMargin is added to the stage timeout so a grant outlives the
// stage it serves, never the reverse: a refresh near the timeout must not be
// refused because the grant died first.
const credentialGrantMargin = 10 * time.Minute

// CredentialGrantTTL sizes a grant for a stage bounded by timeout.
func CredentialGrantTTL(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return timeout + credentialGrantMargin
}

// StageCredentialGrant is one minted grant. Revoke ends it early when the
// stage attempt finishes; it is safe to call more than once.
type StageCredentialGrant struct {
	Endpoint string
	Token    string
	Revoke   func()
}

// StageCredentialGrants mints stage credential-refresh grants. The daemon
// implements it; an executor without one (goobers run, a worker process with
// no daemon API) delivers no grant and its stages behave exactly as before.
type StageCredentialGrants interface {
	MintStageGrant(env apiv1.InvocationEnvelope, capabilities []string, ttl time.Duration) (StageCredentialGrant, error)
}

// appendCredentialGrant adds the grant variables to a goobers-CLI stage's
// environment when a minter is wired and at least one delivered credential
// states an expiry. A credential without one (a PAT) is never refreshed, so a
// stage holding only those gets no grant — exactly today's behavior. A failed
// mint is not a stage failure: the stage runs with its delivered values, as
// it did before grants existed. The returned func revokes the grant.
func (e *ShellExecutor) appendCredentialGrant(stageEnv []string, env apiv1.InvocationEnvelope, injectRunContext bool, timeout time.Duration, registrar credentials.SecretRegistrar) ([]string, func()) {
	noop := func() {}
	if e.CredentialGrants == nil || !injectRunContext {
		return stageEnv, noop
	}
	expiring := expiringDeliveredCapabilities(stageEnv, env.Capabilities)
	if len(expiring) == 0 {
		return stageEnv, noop
	}
	grant, err := e.CredentialGrants.MintStageGrant(env, expiring, CredentialGrantTTL(timeout))
	if err != nil || grant.Token == "" || grant.Endpoint == "" {
		return stageEnv, noop
	}
	registrar.Register([]byte(grant.Token))
	revoke := grant.Revoke
	if revoke == nil {
		revoke = noop
	}
	return append(stageEnv, CredentialEndpointEnvVar+"="+grant.Endpoint, CredentialGrantEnvVar+"="+grant.Token), revoke
}

// expiringDeliveredCapabilities lists the declared capabilities whose
// delivered credential carries a stated expiry in stageEnv.
func expiringDeliveredCapabilities(stageEnv []string, declared []string) []string {
	present := make(map[string]bool, len(stageEnv))
	for _, entry := range stageEnv {
		if name, _, ok := strings.Cut(entry, "="); ok {
			present[name] = true
		}
	}
	var expiring []string
	for _, capabilityName := range declared {
		if present[CredentialEnvVar(capabilityName)] && present[CredentialExpiryEnvVar(capabilityName)] {
			expiring = append(expiring, capabilityName)
		}
	}
	return expiring
}
