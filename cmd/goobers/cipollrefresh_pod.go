package main

import (
	"context"
	"os"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

func podCIPollADOCredentialSource(creds []dispatcher.MintedCredential, scheme, name string, registrar credentials.SecretRegistrar) (providers.ADOCredentialSource, error) {
	initial, err := runtimewiring.MintedADOCredential(creds, scheme, name)
	if err != nil {
		return nil, err
	}
	client, err := credentialPlaneClient([]string{name})
	if err != nil {
		return nil, err
	}
	runID, stage := os.Getenv(dispatcher.EnvRunID), os.Getenv(dispatcher.EnvStage)
	return providers.NewADORefreshingCredentialSource(name, initial, func(ctx context.Context) (providers.ADOCredential, error) {
		resolution, err := client.ResolveStage(ctx, runID, stage, []string{name})
		if err != nil {
			return providers.ADOCredential{}, err
		}
		return runtimewiring.MintedADOCredential(resolution.Credentials, resolution.RepoAuthScheme, name)
	}, registrar)
}

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
		return runtimewiring.MintedValueAndExpiry(resolution.Credentials, capabilityName)
	}
	source, err := runtimewiring.NewCIPollRefreshingToken(capabilityName, minted.Value, expiresAt, refresh, registrar)
	if err != nil {
		return nil
	}
	return source
}
