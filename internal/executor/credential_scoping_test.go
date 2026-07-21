package executor

// End-to-end proof for per-repo credential scoping (MGV-5, #1012). The
// isolation-conformance test (isolation_conformance_test.go, #1092) drives the
// enforcement seam with hand-built per-(gaggle,repo) grants — the representative
// input MGV-5 promised to compute. This test closes the loop from the other
// end: it runs the REAL routing function credentials.RunnerGrants (the exact
// computation cmd/goobers buildCredentials now performs per gaggle) through the
// same live buildStageEnv, and asserts a gaggle's stage holds only its own
// repo token. Together they prove the composition root produces isolated grants
// AND the seam enforces them.

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
)

// runnerInjector builds the runner-owned injector exactly as the per-gaggle
// runner does (credentials.NewInjector over RunnerGrants' output), the
// deterministic-stage credential path in cmd/goobers buildRunnerConfig.
func runnerInjector(t *testing.T, refs []credentials.TokenRef, grants []credentials.Grant, reg credentials.SecretRegistrar) *credentials.Injector {
	t.Helper()
	resolver, err := credentials.NewResolver(refs)
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	inj, err := credentials.NewInjector(resolver, grants, reg)
	if err != nil {
		t.Fatalf("build injector: %v", err)
	}
	return inj
}

// TestPerGaggleRoutingIsolatesRepoTokens: two gaggles under different owners,
// each declaring the SAME plain repo capability (github:pr:write), get their
// grants from the real RunnerGrants routing. Driven through buildStageEnv, each
// gaggle's stage env holds ONLY its own repo token plus the shared agent:model
// default — the other gaggle's token appears nowhere (raw or base64 auth form).
// This is the production model: plain capabilities, one injector per gaggle,
// isolation by construction — no scoped capability keys needed.
func TestPerGaggleRoutingIsolatesRepoTokens(t *testing.T) {
	t.Setenv("GOOBERS_TEST_TOKEN_A", tokenAValue)
	t.Setenv("GOOBERS_TEST_TOKEN_B", tokenBValue)
	t.Setenv("GOOBERS_TEST_TOKEN_SHARED", sharedToken)

	refs := []credentials.TokenRef{
		{Name: "alpha-org/site", Env: "GOOBERS_TEST_TOKEN_A"},
		{Name: "bravo-org/app", Env: "GOOBERS_TEST_TOKEN_B"},
		{Name: "credential:agent:model", Env: "GOOBERS_TEST_TOKEN_SHARED"},
	}
	bindings := []credentials.RepoBinding{
		{Owner: "alpha-org", Name: "site", TokenRef: "alpha-org/site"},
		{Owner: "bravo-org", Name: "app", TokenRef: "bravo-org/app"},
	}
	const pushCap = "github:pr:write"
	sharedOverride := []credentials.Grant{{Capability: "agent:model", Ref: "credential:agent:model"}}

	// The real MGV-5 routing: repo capability -> the gaggle's OWN repo token.
	aGrants := credentials.RunnerGrants(bindings, "alpha-org", "site", []string{pushCap}, sharedOverride)
	bGrants := credentials.RunnerGrants(bindings, "bravo-org", "app", []string{pushCap}, sharedOverride)

	reg := &recordingRegistrar{}
	aInj := runnerInjector(t, refs, aGrants, reg)
	bInj := runnerInjector(t, refs, bGrants, reg)

	declared := []string{pushCap, "agent:model"}
	aEnv, err := buildStageEnv(context.Background(), aInj, declared, reg,
		"run-a", gaggleAName, "implementation", nsA, "/inst", true, nil, nil, nil)
	if err != nil {
		t.Fatalf("assemble gaggle A env: %v", err)
	}
	bEnv, err := buildStageEnv(context.Background(), bInj, declared, reg,
		"run-b", gaggleBName, "implementation", nsB, "/inst", true, nil, nil, nil)
	if err != nil {
		t.Fatalf("assemble gaggle B env: %v", err)
	}

	// A resolves the plain repo capability to A's OWN token, and B to B's —
	// though both stages declared the identical capability string.
	if want := CredentialEnvVar(pushCap) + "=" + tokenAValue; !hasEnv(aEnv, want) {
		t.Fatalf("gaggle A env missing its own repo token %q\nenv: %v", want, aEnv)
	}
	if want := CredentialEnvVar(pushCap) + "=" + tokenBValue; !hasEnv(bEnv, want) {
		t.Fatalf("gaggle B env missing its own repo token %q\nenv: %v", want, bEnv)
	}
	// The shared default resolves for both (unqualified -> shared token).
	if !hasEnv(aEnv, CredentialEnvVar("agent:model")+"="+sharedToken) {
		t.Errorf("gaggle A env missing the shared agent:model default\nenv: %v", aEnv)
	}

	// The load-bearing invariant: the real routing never lets one gaggle's repo
	// token into the other's env — not raw, not base64 auth form.
	assertTokenAbsent(t, "gaggle A env", aEnv, tokenBValue)
	assertTokenAbsent(t, "gaggle B env", bEnv, tokenAValue)
}
