package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

type fakeGrantMinter struct {
	calls   []fakeGrantCall
	revoked int
	err     error
}

type fakeGrantCall struct {
	env          apiv1.InvocationEnvelope
	capabilities []string
	ttl          time.Duration
}

func (f *fakeGrantMinter) MintStageGrant(env apiv1.InvocationEnvelope, capabilities []string, ttl time.Duration) (StageCredentialGrant, error) {
	f.calls = append(f.calls, fakeGrantCall{env: env, capabilities: capabilities, ttl: ttl})
	if f.err != nil {
		return StageCredentialGrant{}, f.err
	}
	return StageCredentialGrant{Endpoint: "http://127.0.0.1:1", Token: "goobers-grant.test-grant-value", Revoke: func() { f.revoked++ }}, nil
}

// grantTestInjector grants repo:push from an expiring source and
// github:issues:write from a static one (a PAT).
func grantTestInjector(t *testing.T) *credentials.Injector {
	t.Helper()
	resolver, err := credentials.NewResolverWithExpiring(nil, nil,
		map[string]credentials.ResolveFunc{"pat": func(context.Context) (string, error) { return "static-pat-value", nil }},
		map[string]credentials.ExpiringResolveFunc{"app": func(context.Context) (string, time.Time, error) {
			return "expiring-app-value", time.Now().Add(20 * time.Minute), nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{
		{Capability: "repo:push", Ref: "app"},
		{Capability: "github:issues:write", Ref: "pat"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	return injector
}

// TestShellExecutor_DeliversACredentialGrantToCLIStagesWithExpiringCredentials
// pins Goobers#6120's delivery rule: a goobers-CLI stage with an expiring
// delivered credential gets GOOBERS_CREDENTIAL_ENDPOINT/GRANT for exactly
// those capabilities, the grant is scrubbed from its output and revoked when
// the attempt returns; a stage holding only PATs, a non-CLI stage, and an
// executor with no minter get neither variable.
func TestShellExecutor_DeliversACredentialGrantToCLIStagesWithExpiringCredentials(t *testing.T) {
	const script = "printf '%s|%s' \"${GOOBERS_CREDENTIAL_ENDPOINT-unset}\" \"${GOOBERS_CREDENTIAL_GRANT-unset}\""
	stub := filepath.Join(t.TempDir(), "goobers")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cli := []string{"goobers", "push-branch"}
	for _, tc := range []struct {
		name         string
		minter       bool
		capabilities []string
		command      []string
		wantGrant    bool
	}{
		{name: "cli stage, expiring credential", minter: true, capabilities: []string{"repo:push", "github:issues:write"}, command: cli, wantGrant: true},
		{name: "cli stage, pat only", minter: true, capabilities: []string{"github:issues:write"}, command: cli},
		{name: "non-cli stage", minter: true, capabilities: []string{"repo:push"}, command: []string{"sh", "-c", script}},
		{name: "no minter", capabilities: []string{"repo:push"}, command: cli},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec, rec := newTestExecutor(t, grantTestInjector(t))
			exec.SelfBin = stub
			minter := &fakeGrantMinter{}
			if tc.minter {
				exec.CredentialGrants = minter
			}
			env := baseEnvelope(t)
			env.RunID, env.Attempt, env.Capabilities = "run-1", 3, tc.capabilities
			exec.DefaultTimeout = 10 * time.Minute
			run := apiv1.DeterministicRun{Command: tc.command}
			result, err := exec.Run(context.Background(), env, run)
			if err != nil || result.Status != apiv1.ResultSuccess {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			stdout := string(rec.recorded["task-1/stdout.log"])
			if !tc.wantGrant {
				if stdout != "unset|unset" || len(minter.calls) != 0 {
					t.Fatalf("stdout = %q, mint calls = %d; want no grant", stdout, len(minter.calls))
				}
				return
			}
			if !strings.HasPrefix(stdout, "http://127.0.0.1:1|") || strings.Contains(stdout, "test-grant-value") {
				t.Fatalf("stdout = %q; want the endpoint and a scrubbed grant", stdout)
			}
			call := minter.calls[0]
			if strings.Join(call.capabilities, ",") != "repo:push" || call.env.Attempt != 3 || call.ttl != 10*time.Minute+10*time.Minute {
				t.Fatalf("mint call = %+v; want repo:push only, attempt 3, timeout+margin", call)
			}
			if minter.revoked != 1 {
				t.Fatalf("revoked %d times, want once when the attempt returned", minter.revoked)
			}
		})
	}
}

// TestShellExecutor_AFailedGrantMintIsNotAStageFailure: the stage runs with
// its delivered values, exactly as before grants existed.
func TestShellExecutor_AFailedGrantMintIsNotAStageFailure(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "goobers")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s' \"${GOOBERS_CREDENTIAL_GRANT-unset}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	exec, rec := newTestExecutor(t, grantTestInjector(t))
	exec.SelfBin = stub
	exec.CredentialGrants = &fakeGrantMinter{err: errors.New("no key")}
	env := baseEnvelope(t)
	env.Capabilities = []string{"repo:push"}
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"goobers", "push-branch"}})
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("Run = %+v, %v", result, err)
	}
	if got := string(rec.recorded["task-1/stdout.log"]); got != "unset" {
		t.Fatalf("stdout = %q, want no grant", got)
	}
}

func TestCredentialGrantTTLOutlivesTheStage(t *testing.T) {
	for timeout, want := range map[time.Duration]time.Duration{
		0:                DefaultTimeout + 10*time.Minute,
		-time.Minute:     DefaultTimeout + 10*time.Minute,
		30 * time.Minute: 40 * time.Minute,
	} {
		if got := CredentialGrantTTL(timeout); got != want {
			t.Errorf("CredentialGrantTTL(%s) = %s, want %s", timeout, got, want)
		}
	}
}

// fixedGrantMinter mints grant on every call.
type fixedGrantMinter struct {
	grant StageCredentialGrant
	calls int
}

func (f *fixedGrantMinter) MintStageGrant(apiv1.InvocationEnvelope, []string, time.Duration) (StageCredentialGrant, error) {
	f.calls++
	return f.grant, nil
}

// TestAppendCredentialGrantDeliversOnlyAUsableGrant: a grant missing its
// token or endpoint is not delivered (nor its token registered), a stage
// without run context asks for none, and a grant minted without a Revoke
// still yields a callable revoke.
func TestAppendCredentialGrantDeliversOnlyAUsableGrant(t *testing.T) {
	stageEnv := []string{
		"PATH=/usr/bin",
		CredentialEnvVar("repo:push") + "=expiring-app-value",
		CredentialExpiryEnvVar("repo:push") + "=2026-01-01T00:00:00Z",
	}
	env := apiv1.InvocationEnvelope{Capabilities: []string{"repo:push"}}
	usable := StageCredentialGrant{Endpoint: "http://127.0.0.1:1", Token: "goobers-grant.usable"}
	cases := map[string]struct {
		grant       StageCredentialGrant
		runContext  bool
		wantDeliver bool
		wantMints   int
	}{
		"usable grant without revoke": {usable, true, true, 1},
		"no token":                    {StageCredentialGrant{Endpoint: "http://127.0.0.1:1"}, true, false, 1},
		"no endpoint":                 {StageCredentialGrant{Token: "goobers-grant.orphan"}, true, false, 1},
		"no run context":              {usable, false, false, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			minter := &fixedGrantMinter{grant: tc.grant}
			registrar := &recordingRegistrar{}
			exec := &ShellExecutor{CredentialGrants: minter}
			got, revoke := exec.appendCredentialGrant(append([]string(nil), stageEnv...), env, tc.runContext, time.Minute, registrar)
			revoke() // must be safe whatever was minted
			if minter.calls != tc.wantMints {
				t.Fatalf("mint calls = %d, want %d", minter.calls, tc.wantMints)
			}
			if !tc.wantDeliver {
				if strings.Join(got, "\n") != strings.Join(stageEnv, "\n") || len(registrar.seen) != 0 {
					t.Fatalf("env = %q, registered %q; want the stage env unchanged and nothing registered", got, registrar.seen)
				}
				return
			}
			want := append(append([]string(nil), stageEnv...), CredentialEndpointEnvVar+"="+usable.Endpoint, CredentialGrantEnvVar+"="+usable.Token)
			if strings.Join(got, "\n") != strings.Join(want, "\n") || !registrar.sawToken(usable.Token) {
				t.Fatalf("env = %q, registered %q; want the grant delivered and registered", got, registrar.seen)
			}
		})
	}
}
