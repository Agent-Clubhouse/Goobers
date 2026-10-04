package credentials

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestChildCeilingWithholdsOpaqueReadPATAndImplicitMCPBeforeResolution(t *testing.T) {
	var resolved []string
	resolver, err := NewResolverWithExpiring(nil, nil, map[string]ResolveFunc{"model": func(context.Context) (string, error) {
		resolved = append(resolved, "model")
		return "model-only-secret", nil
	}, "provider": func(context.Context) (string, error) {
		resolved = append(resolved, "provider")
		return "opaque-write-capable-pat", nil
	}, "mcp": func(context.Context) (string, error) {
		resolved = append(resolved, "mcp")
		return "opaque-mcp-secret", nil
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := NewGooberInjectorWithCredentialKeys(resolver, "coder", []Grant{{Goober: "coder", Capability: "agent:model", Ref: "model"}, {Goober: "coder", Capability: "repo:read", Ref: "provider"}, {Goober: "coder", Capability: "repo:push", Ref: "provider"}, {Goober: "coder", Capability: "mcp:vendor", Ref: "mcp"}}, []string{"mcp:vendor"}, &spyRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	ceiling := NewChildCeiling(false, []string{"agent:model", "repo:read", "repo:push"}, []string{"agent:model", "repo:read", "repo:push"})
	ctx, err := WithChildCeiling(t.Context(), ceiling)
	if err != nil {
		t.Fatal(err)
	}
	ceiling.AllowedKeys[0] = "repo:push" // must not mutate an in-flight host scope
	set, err := injector.Materialize(ctx, []string{"agent:model", "repo:read", "repo:push"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolved, []string{"model"}) {
		t.Fatalf("unauthorized sources resolved: %v", resolved)
	}
	for _, key := range []string{"repo:read", "repo:push", "mcp:vendor"} {
		if _, err := set.Token(ctx, key); !errors.Is(err, ErrUndeclaredCapability) {
			t.Fatalf("credential %s escaped ceiling: %v", key, err)
		}
	}
	if err := RefuseUnisolatedChildProcess(ctx); !errors.Is(err, ErrChildAuthenticationIsolation) {
		t.Fatal("local HOME/ambient path accepted")
	}
	if err := RefuseUnisolatedChildProcess(t.Context()); err != nil {
		t.Fatal("ordinary execution changed")
	}
}

func TestChildPublicationDelegationStillIntersectsEnclosingGrant(t *testing.T) {
	ceiling := NewChildCeiling(true, []string{"agent:model", "repo:read"}, []string{"agent:model", "repo:read", "repo:push", "provider:pr:write", "mcp:vendor"})
	if !slices.Equal(ceiling.AllowedKeys, []string{"agent:model", "repo:read"}) {
		t.Fatalf("delegation widened parent: %+v", ceiling)
	}
	if err := ceiling.Validate(); err != nil {
		t.Fatal(err)
	}
	ceiling.AllowPublication = false
	if err := ceiling.Validate(); err == nil {
		t.Fatal("read PAT accepted without publication permission")
	}
}
