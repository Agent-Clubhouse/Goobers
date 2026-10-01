package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// cipollrefresh.go ends the #3489 snapshot in both ci-poll paths
// (Goobers#6120 phase 1). ci-poll runs in-process — in the daemon on the local
// substrate, in dispatch-exec in a pod — and used to resolve its PR token once
// and poll with it for the whole window, so a poll that outlived the token
// failed on its next request. Both paths now poll through a
// credentials.RefreshingToken whose refresh goes back to the source the first
// value came from: the daemon's own injector locally, the credential plane
// (under the pod's own token) in a pod.

// ciPollTokenSource resolves capabilityName through injector and wraps the
// value in a refreshing source (newCIPollRefreshingToken): refreshed ahead of
// a stated expiry, and re-resolved after a 401 whether or not it states one.
func ciPollTokenSource(ctx context.Context, injector *credentials.Injector, capabilityName string, registrar credentials.SecretRegistrar) (string, providers.TokenSource, error) {
	token, expiresAt, err := materializeCapability(ctx, injector, capabilityName)
	if err != nil {
		return "", nil, err
	}
	refresh := func(ctx context.Context) (string, time.Time, error) {
		return materializeCapability(ctx, injector, capabilityName)
	}
	source, err := newCIPollRefreshingToken(capabilityName, token, expiresAt, refresh, registrar)
	if err != nil {
		return "", nil, err
	}
	return token, source, nil
}

// materializeCapability resolves one capability through injector: a fresh
// value per call (the injector's resolver re-mints a GitHub App token near
// its expiry), registered with the injector's scrubber registrar.
func materializeCapability(ctx context.Context, injector *credentials.Injector, capabilityName string) (string, time.Time, error) {
	set, err := injector.Materialize(ctx, []string{capabilityName})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
	}
	token, err := set.Token(ctx, capabilityName)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("resolve ci-poll credential: %w", err)
	}
	expiresAt, _ := set.Expiry(capabilityName)
	return token, expiresAt, nil
}

// localCIPollGitHubPoller is the local ci-poll's GitHub poller: through the
// newPRPoller test seam when set, else a provider that resolves its token per
// request from the injector-backed refreshing source (when the value expires)
// and re-resolves it once on 401.
func localCIPollGitHubPoller(ctx context.Context, injector *credentials.Injector, capabilityName string, registrar credentials.SecretRegistrar) (executor.PRPoller, error) {
	token, source, err := ciPollTokenSource(ctx, injector, capabilityName, registrar)
	if err != nil {
		return nil, err
	}
	if newPRPoller != nil {
		return newPRPoller(token), nil
	}
	if source == nil {
		return providers.NewGitHubProvider(token), nil
	}
	return providers.NewGitHubProvider(token, providers.WithTokenSource(source)), nil
}

// podCIPollTokenSource wraps the value the pod resolved at stage start in a
// refreshing source that re-resolves through the credential plane under the
// pod's own token — the same call that produced it — ahead of a stated expiry
// and after a 401 (newCIPollRefreshingToken). Nil when no value was resolved.
func podCIPollTokenSource(creds []dispatcher.MintedCredential, capabilityName string, registrar credentials.SecretRegistrar) providers.TokenSource {
	var minted dispatcher.MintedCredential
	for _, cred := range creds {
		if cred.Capability == capabilityName {
			minted = cred
		}
	}
	if minted.Value == "" {
		return nil
	}
	var expiresAt time.Time
	if minted.ExpiresAt != nil {
		expiresAt = *minted.ExpiresAt
	}
	refresh := func(ctx context.Context) (string, time.Time, error) {
		client, err := credentialPlaneClient([]string{capabilityName})
		if err != nil {
			return "", time.Time{}, err
		}
		resolution, err := client.ResolveStage(ctx, os.Getenv(dispatcher.EnvRunID), os.Getenv(dispatcher.EnvStage), []string{capabilityName})
		if err != nil {
			return "", time.Time{}, err
		}
		return mintedValueAndExpiry(resolution.Credentials, capabilityName)
	}
	source, err := newCIPollRefreshingToken(capabilityName, minted.Value, expiresAt, refresh, registrar)
	if err != nil {
		return nil
	}
	return source
}

// newCIPollRefreshingToken is ci-poll's token source. A value with a stated
// expiry is refreshed ahead of it and once after a 401 (Goobers#6120). A value
// without one (a PAT) is never refreshed proactively, but is still re-resolved
// after a 401 — once per ci-poll unauthorized retry (#6154), whose bound
// limits the attempts — rather than on every poll request.
func newCIPollRefreshingToken(capabilityName, token string, expiresAt time.Time, refresh credentials.RefreshFunc, registrar credentials.SecretRegistrar) (*credentials.RefreshingToken, error) {
	if expiresAt.IsZero() {
		return credentials.NewInvalidationRefreshingToken(capabilityName, token, refresh, registrar)
	}
	return credentials.NewRefreshingToken(capabilityName, token, expiresAt, refresh, registrar)
}

func mintedValueAndExpiry(creds []dispatcher.MintedCredential, capabilityName string) (string, time.Time, error) {
	for _, cred := range creds {
		if cred.Capability != capabilityName {
			continue
		}
		var expiresAt time.Time
		if cred.ExpiresAt != nil {
			expiresAt = *cred.ExpiresAt
		}
		return cred.Value, expiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("the credential plane returned no value for %q", capabilityName)
}
