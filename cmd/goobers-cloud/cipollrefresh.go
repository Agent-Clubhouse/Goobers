package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

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

func newCIPollRefreshingToken(capabilityName, token string, expiresAt time.Time, refresh credentials.RefreshFunc, registrar credentials.SecretRegistrar) (*credentials.RefreshingToken, error) {
	if expiresAt.IsZero() {
		return credentials.NewInvalidationRefreshingToken(capabilityName, token, refresh, registrar)
	}
	return credentials.NewRefreshingToken(capabilityName, token, expiresAt, refresh, registrar)
}
