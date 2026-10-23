package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTrustedLocalRequiresHostCeilingAndPrivateDirectory(t *testing.T) {
	if _, err := WithTrustedLocalProcess(t.Context(), t.TempDir()); err == nil {
		t.Fatal("accepted launch without child credential ceiling")
	}
	ctx, err := WithChildCeiling(t.Context(), NewChildCeiling(false, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", filepath.Join(t.TempDir(), "missing")} {
		if _, err := WithTrustedLocalProcess(ctx, path); err == nil {
			t.Fatalf("accepted home %q", path)
		}
	}
	if !errors.Is(RefuseUnisolatedChildProcess(ctx), ErrChildAuthenticationIsolation) {
		t.Fatal("plain child context authorized host launch")
	}
	ctx, err = WithTrustedLocalProcess(ctx, t.TempDir())
	if err != nil || RefuseUnisolatedChildProcess(ctx) != nil {
		t.Fatalf("admitted private context refused: %v", err)
	}
}

func TestTrustedLocalEnvironmentCannotRestoreAmbientAuthentication(t *testing.T) {
	home := t.TempDir()
	ctx, _ := WithChildCeiling(t.Context(), NewChildCeiling(false, nil, nil))
	ctx, err := WithTrustedLocalProcess(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	ambient := []string{"PATH=/bin", "HOME=/ambient", "HTTP_PROXY=https://user:password@proxy",
		"GH_TOKEN=provider-secret", "GIT_CONFIG_COUNT=7", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=ambient-helper", "SSH_AUTH_SOCK=/agent", "CODEX_HOME=/stored-login",
		"DOTNET_CLI_HOME=/stored-nuget", "XDG_CONFIG_HOME=/ambient-config"}
	base := TrustedLocalBaseEnvironment(ctx, ambient)
	for _, entry := range base {
		if strings.Contains(entry, "ambient") || strings.Contains(entry, "secret") ||
			strings.Contains(entry, "stored") || strings.Contains(entry, "password") {
			t.Fatalf("ambient authentication inherited: %s", entry)
		}
	}
	// Authored env comes after the base. Reapply discovery policy last.
	env := TrustedLocalEnvironment(ctx, append(base, "HOME=/override", "GIT_CONFIG_COUNT=5", "SSH_AUTH_SOCK=/agent", "RESULT=ok"))
	for _, want := range []string{"HOME=" + home, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_VALUE_0=", "RESULT=ok"} {
		if !slices.Contains(env, want) {
			t.Fatalf("missing %q in %v", want, env)
		}
	}
	if !slices.Equal(TrustedLocalEnvironment(t.Context(), ambient), ambient) ||
		!slices.Equal(TrustedLocalBaseEnvironment(t.Context(), ambient), ambient) {
		t.Fatal("ordinary environment changed")
	}
}

func TestTrustedLocalProcessWithholdsPublicationCredentials(t *testing.T) {
	parent := []string{"agent:model", "repo:push", "provider:pr:write"}
	ctx, err := WithChildCeiling(t.Context(), NewChildCeiling(true, parent, parent))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = WithTrustedLocalProcess(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ceiling, ok := ChildCeilingFromContext(ctx)
	if !ok || ceiling.AllowPublication || !slices.Equal(ceiling.AllowedKeys, []string{"agent:model"}) {
		t.Fatalf("authored process inherited publication: %+v", ceiling)
	}
}

func TestTrustedLocalCredentialReferenceCannotWidenParentGrant(t *testing.T) {
	ctx, err := WithChildCeiling(t.Context(), NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = WithTrustedLocalProcess(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	injector, registrar := newTestInjector(t, []Grant{{Capability: "agent:model", Ref: "github-issues"}})
	if _, err := injector.Materialize(ctx, []string{"agent:model"}); !errors.Is(err, ErrChildAuthenticationIsolation) {
		t.Fatalf("unbound credential resolved: %v", err)
	}
	bound, err := WithTrustedLocalModelCredential(ctx, "repo-push")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := injector.Materialize(bound, []string{"agent:model"}); !errors.Is(err, ErrChildAuthenticationIsolation) {
		t.Fatalf("different stored ref resolved: %v", err)
	}
	if len(registrar.registered) != 0 {
		t.Fatal("undelegated secret materialized before refusal")
	}
	bound, err = WithTrustedLocalModelCredential(ctx, "github-issues")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := injector.Materialize(bound, []string{"agent:model"}); err != nil {
		t.Fatalf("parent's exact ref refused: %v", err)
	}
	ambient, err := WithTrustedLocalProcess(ctx, t.TempDir(), TrustedLocalAuthentication{Harness: "copilot", Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithTrustedLocalModelCredential(ambient, "github-issues"); !errors.Is(err, ErrChildAuthenticationIsolation) {
		t.Fatal("ambient parent widened to explicit stored key")
	}
	if _, err := injector.Materialize(ambient, []string{"agent:model"}); !errors.Is(err, ErrChildAuthenticationIsolation) {
		t.Fatal("ambient child resolved explicit stored key")
	}
}
