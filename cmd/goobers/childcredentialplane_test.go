package main

import (
	"context"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildCredentialPlaneRechecksRealQueueSourceCurrentPolicyAndCancellation(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	service := &daemonCredentialService{layout: f.launcher.layout, childCredentials: f.launcher.credentialCeiling}
	ctx, lease, err := service.applyChildCredentialCeiling(t.Context(), pinnedStage{identity: id})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	if f.authority.held.Load() != 1 {
		t.Fatal("current authority lease ended before materialization")
	}
	keys := credentials.FilterChildCredentialKeys(ctx, []string{"agent:model", "repo:read", "repo:push", "mcp:vendor"})
	if !slices.Equal(keys, []string{"agent:model"}) || credentials.RefuseUnisolatedChildProcess(ctx) == nil {
		t.Fatalf("child credential restrictions absent: keys=%v", keys)
	}
	if err := lease.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := lease.finish(ctx); err == nil {
		t.Fatal("cancellation during secret resolution returned credentials")
	}
	if _, _, err := service.applyChildCredentialCeiling(t.Context(), pinnedStage{identity: id}); err == nil {
		t.Fatal("cancelled family resolved credentials")
	}
}

func TestChildCredentialBrokerWithholdsProviderMCPAndConnectorBeforeResolution(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	service := &daemonCredentialService{layout: f.launcher.layout, childCredentials: f.launcher.credentialCeiling, config: &instance.Config{}, shared: journal.NewRegistryScrubber()}
	pinned := pinnedStage{identity: id, defs: credentialPlaneDefinitions{Scopes: map[string]credentialGaggleScope{id.Gaggle: {Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub}}}}, profile: stageProfile{goober: "coder", harness: "claude", capabilities: []string{"agent:model", "repo:read", "repo:push", "telemetry:read"}, implicitKeys: []string{"repo:push", "mcp:vendor"}, externalTelemetryConnector: "must-not-resolve"}}
	var resolved []string
	service.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		functions := map[string]credentials.ResolveFunc{}
		grants := []credentials.Grant{}
		for _, key := range []string{"agent:model", "repo:read", "repo:push", "mcp:vendor"} {
			functions[key] = func(context.Context) (string, error) { resolved = append(resolved, key); return "secret-" + key, nil }
			grants = append(grants, credentials.Grant{Capability: key, Ref: key})
		}
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, functions, nil)
		return resolver, grants, err
	}
	ctx, lease, err := service.applyChildCredentialCeiling(t.Context(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	result, err := service.mintStageCredentials(ctx, pinned, pinned.profile.capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolved, []string{"agent:model"}) || len(result.minted) != 1 || result.minted[0].Capability != "agent:model" {
		t.Fatalf("broker widened child: sources=%v result=%+v", resolved, result.minted)
	}
}

func TestChildCredentialPlaneRefusesAbsentVerifierAndRevokedCurrentAuthority(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	service := &daemonCredentialService{layout: f.launcher.layout}
	if _, _, err := service.applyChildCredentialCeiling(t.Context(), pinnedStage{identity: id}); err == nil {
		t.Fatal("missing source verifier permitted credentials")
	}
	service.childCredentials = f.launcher.credentialCeiling
	f.authority.revoked.Store(true)
	if _, _, err := service.applyChildCredentialCeiling(t.Context(), pinnedStage{identity: id}); err == nil {
		t.Fatal("revoked live policy permitted credentials")
	}
}
