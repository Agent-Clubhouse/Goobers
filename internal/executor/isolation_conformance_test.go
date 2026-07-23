package executor

// Multi-gaggle isolation-conformance test (MGV-9, #1092).
//
// Design: docs/design/v1/multi-gaggle-validation.md §4.5 (G5). V1's isolation
// posture is "isolation by construction, proven by an automated conformance
// test" — OS-level enforcement (Seatbelt/bubblewrap #165-#167, per-gaggle
// workload identity #685) is tracked separately as outstanding security debt.
// This file is that conformance artifact: it makes the isolation claim
// checkable in CI on every change and catches a scoping regression even before
// the OS sandbox rungs land.
//
// What it asserts, mechanically, over a two-gaggle fixture (A and B, distinct
// owners + distinct per-repo tokens): a stage running in gaggle A
//   (a) has NO gaggle-B token anywhere in its resolved subprocess env;
//   (b) fails CLOSED when it asks for a capability scoped to gaggle B —
//       ErrUndeclaredCapability, never silently resolving A's own token;
//   (c) sees worktree paths and repo targets referencing ONLY A's repos, with
//       no credentialed path to B's repos.
//
// Scope boundary (deliberate, per design §6 sequencing "MGV-9 ... asserts the
// scoping invariant ... before MGV-5"): this drives the real enforcement seam
// — credentials.NewGooberInjector, the live buildStageEnv used by every stage,
// and instance.Layout — with a fixture that supplies each gaggle's own scoped
// grants. Those per-(gaggle,repo,capability) grants are exactly the input
// MGV-5 (#1012, per-repo credential scoping) will compute at the composition
// root (cmd/goobers buildCredentials, today still instance-global). Feeding the
// enforcement seam that representative input proves the seam holds the line;
// when MGV-5 wires real per-gaggle grants through buildCredentials, this test
// is the regression harness that catches any leak.

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

// A two-gaggle fixture under different owners. Capability names are
// repo-qualified (`cap@owner/repo`) to mirror the design's (gaggle, repo,
// capability) scope key (§2) and make "a capability scoped to gaggle B"
// concrete: it is a distinct grant key A neither declares nor is granted.
const (
	gaggleAName  = "alpha"
	gaggleBName  = "bravo"
	nsA          = "alpha-web/" // gaggle A's run-branch namespace (#965/#1010)
	nsB          = "bravo-app/" // gaggle B's run-branch namespace
	tokenAValue  = "tokenA-ALPHA-secret-000"
	tokenBValue  = "tokenB-BRAVO-secret-999"
	sharedToken  = "shared-agent-model-token-abc" // the instance-wide default (§2)
	capPushA     = "github:pr:write@alpha-org/site"
	capPushB     = "github:pr:write@bravo-org/app"
	capAgentBoth = "agent:model" // unqualified → shared default, legitimate cross-gaggle
)

// recordingRegistrar captures every secret the injector resolves so the test
// can assert scrubber hygiene (every materialized token is registered) in
// addition to the isolation invariants.
type recordingRegistrar struct{ seen [][]byte }

func (r *recordingRegistrar) Register(secret []byte) {
	r.seen = append(r.seen, append([]byte(nil), secret...))
}

func (r *recordingRegistrar) sawToken(token string) bool {
	for _, s := range r.seen {
		if string(s) == token {
			return true
		}
	}
	return false
}

// gaggleInjector builds the goober-scoped injector for one gaggle exactly as
// the runner does (credentials.NewGooberInjector). grants are the gaggle's own
// scoped grants — the MGV-5 end-state input — bound to that gaggle's goober
// identity, so no other gaggle's grant is ever reachable through it.
func gaggleInjector(t *testing.T, goober string, refs []credentials.TokenRef, grants []credentials.Grant, reg credentials.SecretRegistrar) *credentials.Injector {
	t.Helper()
	resolver, err := credentials.NewResolver(refs)
	if err != nil {
		t.Fatalf("build resolver for %s: %v", goober, err)
	}
	inj, err := credentials.NewGooberInjector(resolver, goober, grants, reg)
	if err != nil {
		t.Fatalf("build goober injector for %s: %v", goober, err)
	}
	return inj
}

// fixtureGrants wires the two gaggles' token refs (env-backed, distinct
// values) and their goober-scoped grants. Both gaggles legitimately share the
// unqualified agent:model default; each holds only its own repo-scoped push
// grant. Returns the two goober injectors and the recording registrar.
func fixtureGrants(t *testing.T) (a, b *credentials.Injector, reg *recordingRegistrar) {
	t.Helper()
	t.Setenv("GOOBERS_TEST_TOKEN_A", tokenAValue)
	t.Setenv("GOOBERS_TEST_TOKEN_B", tokenBValue)
	t.Setenv("GOOBERS_TEST_TOKEN_SHARED", sharedToken)

	refs := []credentials.TokenRef{
		{Name: "ref-a", Env: "GOOBERS_TEST_TOKEN_A"},
		{Name: "ref-b", Env: "GOOBERS_TEST_TOKEN_B"},
		{Name: "ref-shared", Env: "GOOBERS_TEST_TOKEN_SHARED"},
	}
	reg = &recordingRegistrar{}

	// Gaggle A's goober: only A's repo-scoped push token + the shared default.
	aGrants := []credentials.Grant{
		{Goober: "goober-a", Capability: capPushA, Ref: "ref-a"},
		{Goober: "goober-a", Capability: capAgentBoth, Ref: "ref-shared"},
	}
	// Gaggle B's goober: only B's repo-scoped push token + the shared default.
	bGrants := []credentials.Grant{
		{Goober: "goober-b", Capability: capPushB, Ref: "ref-b"},
		{Goober: "goober-b", Capability: capAgentBoth, Ref: "ref-shared"},
	}
	a = gaggleInjector(t, "goober-a", refs, aGrants, reg)
	b = gaggleInjector(t, "goober-b", refs, bGrants, reg)
	return a, b, reg
}

// TestIsolationConformance_NoCrossGaggleTokenInEnv asserts criterion (a): a
// stage in gaggle A, whose resolved subprocess env is assembled by the same
// buildStageEnv every live stage runs through, holds A's token and the shared
// default — and gaggle B's token appears NOWHERE in that env, under any var,
// raw or base64-encoded (the form it would take inside a git auth header). It
// also covers the per-gaggle run-context injected alongside credentials
// (branchNamespace, #965/#1010): A's env carries A's namespace, never B's.
func TestIsolationConformance_NoCrossGaggleTokenInEnv(t *testing.T) {
	aInj, bInj, reg := fixtureGrants(t)

	// injectRunContext=true so the per-gaggle run-context (gaggle, workflow, and
	// the #965/#1010 branch namespace) is assembled into the env too — the same
	// path a live goobers-CLI stage takes — letting the isolation assertion cover
	// that dimension, not only the credential vars.
	aEnv, err := buildStageEnv(context.Background(), aInj,
		[]string{capPushA, capAgentBoth}, reg,
		"run-a", gaggleAName, "implementation", nsA, "/inst", true, nil, nil, nil)
	if err != nil {
		t.Fatalf("assemble gaggle A env: %v", err)
	}
	bEnv, err := buildStageEnv(context.Background(), bInj,
		[]string{capPushB, capAgentBoth}, reg,
		"run-b", gaggleBName, "implementation", nsB, "/inst", true, nil, nil, nil)
	if err != nil {
		t.Fatalf("assemble gaggle B env: %v", err)
	}

	// A holds its own repo token, keyed under the deterministic cred var.
	wantA := CredentialEnvVar(capPushA) + "=" + tokenAValue
	if !hasEnv(aEnv, wantA) {
		t.Fatalf("gaggle A env missing its own push token %q\nenv: %v", wantA, aEnv)
	}
	// The shared default resolves for A too (§2: unqualified → shared token).
	if !hasEnv(aEnv, CredentialEnvVar(capAgentBoth)+"="+sharedToken) {
		t.Errorf("gaggle A env missing the shared agent:model default\nenv: %v", aEnv)
	}

	// The load-bearing assertion: gaggle B's token is not present anywhere in
	// A's env — not as a value, not as a substring, not base64-encoded.
	assertTokenAbsent(t, "gaggle A env", aEnv, tokenBValue)
	// And symmetrically, A's token never leaks into B's env.
	assertTokenAbsent(t, "gaggle B env", bEnv, tokenAValue)

	// Per-gaggle run-context isolation: A's env carries A's own branch namespace,
	// and gaggle B's namespace appears nowhere in it (and vice-versa).
	if !hasEnv(aEnv, BranchNamespaceEnvVar+"="+nsA) {
		t.Errorf("gaggle A env missing its own branch namespace %q\nenv: %v", nsA, aEnv)
	}
	for _, kv := range aEnv {
		if strings.Contains(kv, nsB) {
			t.Fatalf("gaggle A env leaked gaggle B's branch namespace %q: %q", nsB, kv)
		}
	}
	for _, kv := range bEnv {
		if strings.Contains(kv, nsA) {
			t.Fatalf("gaggle B env leaked gaggle A's branch namespace %q: %q", nsA, kv)
		}
	}

	// Scrubber hygiene: every token that WAS materialized is registered for
	// scrubbing (nothing reaches the env without also reaching the scrubber).
	for _, tok := range []string{tokenAValue, tokenBValue, sharedToken} {
		if !reg.sawToken(tok) {
			t.Errorf("materialized token %q was never registered with the scrubber", tok)
		}
	}
}

// assertTokenAbsent fails if token appears in any env entry — as a raw
// substring or base64-encoded (the encoding gitAuthEnv uses when it folds a
// token into an http.extraheader Authorization value).
func assertTokenAbsent(t *testing.T, label string, env []string, token string) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	for _, kv := range env {
		if strings.Contains(kv, token) {
			t.Fatalf("%s leaked a foreign gaggle token (raw): entry %q contains %q", label, kv, token)
		}
		if strings.Contains(kv, encoded) {
			t.Fatalf("%s leaked a foreign gaggle token (base64 auth form): entry %q", label, kv)
		}
	}
}

// TestIsolationConformance_CrossGaggleCapabilityFailsClosed asserts criterion
// (b): a stage in gaggle A that reaches for a capability scoped to gaggle B
// fails closed — its materialized Set never declared B's scope key, so Token
// returns ErrUndeclaredCapability with an empty token, rather than silently
// substituting A's own token for the wrong repo.
func TestIsolationConformance_CrossGaggleCapabilityFailsClosed(t *testing.T) {
	aInj, _, reg := fixtureGrants(t)
	ctx := context.Background()

	// A materializes exactly its own declared capabilities.
	set, err := aInj.Materialize(ctx, []string{capPushA, capAgentBoth})
	if err != nil {
		t.Fatalf("materialize gaggle A set: %v", err)
	}

	// Reaching for gaggle B's scope key fails closed — never resolves.
	tok, err := set.Token(ctx, capPushB)
	if err == nil {
		t.Fatalf("gaggle A resolved a gaggle-B-scoped capability %q -> %q; expected fail-closed", capPushB, tok)
	}
	if tok != "" {
		t.Fatalf("fail-closed resolution must return an empty token, got %q", tok)
	}
	if got := err.Error(); !strings.Contains(got, "not declared") {
		t.Errorf("expected an undeclared-capability error for %q, got: %v", capPushB, got)
	}
	// Belt-and-suspenders: the returned token is never A's token either.
	if tok == tokenAValue {
		t.Fatalf("gaggle A silently substituted its OWN token for a B-scoped capability")
	}
	_ = reg

	// A's own scope key still resolves normally — isolation does not break the
	// legitimate path.
	own, err := set.Token(ctx, capPushA)
	if err != nil {
		t.Fatalf("gaggle A failed to resolve its OWN capability %q: %v", capPushA, err)
	}
	if own != tokenAValue {
		t.Fatalf("gaggle A resolved its own capability to %q, want its own token", own)
	}
}

// TestIsolationConformance_WorktreeAndRemotesReferenceOnlyOwnGaggle asserts
// criterion (c): a stage in gaggle A sees worktree/runtime paths and repo
// targets that reference only A's repos — no path or credentialed reach into
// gaggle B's repos.
func TestIsolationConformance_WorktreeAndRemotesReferenceOnlyOwnGaggle(t *testing.T) {
	base := instance.NewLayout("/instances/demo")
	aLayout := base.ForGaggle(gaggleAName)
	bLayout := base.ForGaggle(gaggleBName)

	// Per-gaggle runtime roots are disjoint: gaggle A's managed working copies
	// and run journals live under gaggles/alpha, B's under gaggles/bravo.
	// Neither can name a path into the other's tree.
	aWork, bWork := aLayout.WorkcopiesDir(), bLayout.WorkcopiesDir()
	aRuns, bRuns := aLayout.RunsDir(), bLayout.RunsDir()
	aWorkSlash, bWorkSlash := filepath.ToSlash(aWork), filepath.ToSlash(bWork)
	if aWork == bWork || aRuns == bRuns {
		t.Fatalf("per-gaggle runtime roots collide: workcopies %q/%q runs %q/%q", aWork, bWork, aRuns, bRuns)
	}
	if strings.HasPrefix(aWorkSlash, bWorkSlash+"/") || strings.HasPrefix(bWorkSlash, aWorkSlash+"/") {
		t.Fatalf("one gaggle's workcopies dir nests inside the other's: %q vs %q", aWork, bWork)
	}
	if !strings.Contains(aWorkSlash, "/gaggles/"+gaggleAName+"/") {
		t.Fatalf("gaggle A workcopies %q is not scoped under its own gaggle dir", aWork)
	}
	if strings.Contains(aWork, gaggleBName) || strings.Contains(aRuns, gaggleBName) {
		t.Fatalf("gaggle A runtime paths reference gaggle B: workcopies=%q runs=%q", aWork, aRuns)
	}

	// A gaggle's managed working copies for ANY repo it targets are rooted under
	// its own gaggle dir — so even the on-disk clone of a repo A shares by name
	// with B (were that to happen) lands under gaggles/A, never in B's tree.
	// (The per-(gaggle,repo) credentialed URL routing itself becomes assertable
	// here once MGV-5 (#1012) lands the routing function; today the provable
	// invariant is the disjoint per-gaggle on-disk root asserted above.)
	if !strings.HasPrefix(aWorkSlash, filepath.ToSlash(aLayout.GagglesDir())+"/"+gaggleAName+"/") {
		t.Fatalf("gaggle A workcopies %q escaped its own gaggle root", aWork)
	}

	// The credential backing A's git operations is A's token, not B's —
	// so even the auth material on any remote A touches is gaggle-local.
	aInj, _, reg := fixtureGrants(t)
	set, err := aInj.Materialize(context.Background(), []string{capPushA})
	if err != nil {
		t.Fatalf("materialize gaggle A push set: %v", err)
	}
	pushTok, err := set.Token(context.Background(), capPushA)
	if err != nil {
		t.Fatalf("gaggle A push token: %v", err)
	}
	if pushTok != tokenAValue {
		t.Fatalf("gaggle A git-push token is %q, want its own token (no credentialed path to B)", pushTok)
	}
	if reg.sawToken(tokenBValue) {
		// fixtureGrants built both injectors, but A's Materialize above must
		// only ever have registered A's + shared tokens — never B's.
		// (B's token enters reg only if B's injector materialized it, which it
		// did not in this test.)
		t.Fatalf("gaggle B token was registered while resolving only gaggle A's push credential")
	}
}
