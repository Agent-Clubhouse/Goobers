package main

import (
	"context"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/stageenv"
)

// stagecredentials.go is the stage side of mid-stage credential refresh
// (Goobers#6120 phase 1). A deterministic stage that was handed a
// credential-refresh grant (GOOBERS_CREDENTIAL_ENDPOINT +
// GOOBERS_CREDENTIAL_GRANT) replaces each delivered value that states an
// expiry with a credentials.RefreshingToken: it re-resolves one capability
// through the daemon's refresh route when the value is within five minutes of
// its expiry, and once after the provider answers 401. Everything else — a
// value without an expiry (a PAT), a stage without a grant, a caller that
// passed its own explicit token — keeps the static delivered value, exactly as
// before grants existed.

// stageRefreshingTokens holds one RefreshingToken per capability for this
// process, so every provider and git invocation of the stage shares one
// refresh (and one rate-limit budget) per capability.
var stageRefreshingTokens sync.Map // capability name -> *credentials.RefreshingToken

// stageRefreshingToken returns the refreshing source for the value delivered
// for cap, or nil when the static value must be used: no grant, no stated
// expiry, or token is not the delivered value.
func stageRefreshingToken(cap capability.Capability, token string) *credentials.RefreshingToken {
	return stageRefreshingTokenFrom(nil, cap, token)
}

// stageRefreshingTokenFrom is stageRefreshingToken reading the delivered
// variables from env (nil: the process environment).
func stageRefreshingTokenFrom(env stageenv.Lookup, cap capability.Capability, token string) *credentials.RefreshingToken {
	endpoint := env.Get(executor.CredentialEndpointEnvVar)
	grant := env.Get(executor.CredentialGrantEnvVar)
	if endpoint == "" || grant == "" || token == "" || token != env.Get(executor.CredentialEnvVar(string(cap))) {
		return nil
	}
	if cached, ok := stageRefreshingTokens.Load(string(cap)); ok {
		return cached.(*credentials.RefreshingToken)
	}
	expiresAt, ok := capability.ParseCredentialExpiry(env.Get(capability.CredentialExpiryEnvVar(string(cap))))
	if !ok {
		return nil
	}
	client := &dispatcher.CredentialRefreshClient{BaseURL: endpoint, Grant: grant}
	refreshing, err := credentials.NewRefreshingToken(string(cap), token, expiresAt, grantRefresh(client, string(cap)), nil)
	if err != nil {
		return nil
	}
	actual, _ := stageRefreshingTokens.LoadOrStore(string(cap), refreshing)
	return actual.(*credentials.RefreshingToken)
}

// grantRefresh re-resolves capabilityName through the refresh route.
func grantRefresh(client *dispatcher.CredentialRefreshClient, capabilityName string) credentials.RefreshFunc {
	return func(ctx context.Context) (string, time.Time, error) {
		minted, err := client.Refresh(ctx, capabilityName)
		if err != nil {
			return "", time.Time{}, err
		}
		var expiresAt time.Time
		if minted.ExpiresAt != nil {
			expiresAt = *minted.ExpiresAt
		}
		return minted.Value, expiresAt, nil
	}
}

// currentStageToken is the value to send right now for cap: the refreshing
// source's current (proactively refreshed) value when the stage holds a grant
// for it, else token unchanged. For callers without an error path of their
// own (a git environment); a failed refresh keeps the delivered value.
func currentStageToken(cap capability.Capability, token string) string {
	refreshing := stageRefreshingToken(cap, token)
	if refreshing == nil {
		return token
	}
	current, err := refreshing.Token(context.Background())
	if err != nil {
		return token
	}
	return current
}
