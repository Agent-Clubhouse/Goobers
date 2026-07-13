package credentials

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// spyRegistrar records every secret it is asked to register, standing in for
// journal.RegistryScrubber in tests that don't need the real journal.
type spyRegistrar struct {
	registered [][]byte
}

func (s *spyRegistrar) Register(secret []byte) {
	s.registered = append(s.registered, append([]byte(nil), secret...))
}

// scrub replicates RegistryScrubber.Scrub closely enough to prove the
// contract: every value passed to Register is fully removable from
// arbitrary bytes.
func (s *spyRegistrar) scrub(b []byte) []byte {
	out := b
	for _, v := range s.registered {
		out = bytes.ReplaceAll(out, v, []byte("[REDACTED]"))
	}
	return out
}

func newTestInjector(t *testing.T, grants []Grant) (*Injector, *spyRegistrar) {
	t.Helper()
	t.Setenv("GH_ISSUES_TOKEN", "ghp_canaryTokenValue123456789")
	t.Setenv("REPO_PUSH_TOKEN", "ghp_pushTokenValue987654321")
	resolver, err := NewResolver([]TokenRef{
		{Name: "github-issues", Env: "GH_ISSUES_TOKEN"},
		{Name: "repo-push", Env: "REPO_PUSH_TOKEN"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	reg := &spyRegistrar{}
	inj, err := NewInjector(resolver, grants, reg)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return inj, reg
}

func TestMaterializeOnlyGrantsDeclaredCapabilities(t *testing.T) {
	inj, _ := newTestInjector(t, []Grant{
		{Capability: "github:issues:write", Ref: "github-issues"},
		{Capability: "repo:push", Ref: "repo-push"},
	})

	set, err := inj.Materialize(context.Background(), []string{"github:issues:write"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	tok, err := set.Token(context.Background(), "github:issues:write")
	if err != nil || tok != "ghp_canaryTokenValue123456789" {
		t.Fatalf("Token(declared) = %q, %v; want the resolved value, nil error", tok, err)
	}

	// repo:push was never declared for this stage — no credential exists for
	// it, even though a grant is configured for the capability generally.
	if _, err := set.Token(context.Background(), "repo:push"); !errors.Is(err, ErrUndeclaredCapability) {
		t.Fatalf("Token(undeclared) error = %v, want ErrUndeclaredCapability", err)
	}
}

func TestMaterializeFailsClosedOnUnresolvableGrantedCapability(t *testing.T) {
	resolver, err := NewResolver([]TokenRef{{Name: "github-issues", Env: "GH_TOKEN_UNSET_FOR_TEST_XYZ"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	inj, err := NewInjector(resolver, []Grant{{Capability: "github:issues:write", Ref: "github-issues"}}, &spyRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	if _, err := inj.Materialize(context.Background(), []string{"github:issues:write"}); err == nil {
		t.Fatal("Materialize: want error when a declared, granted capability cannot be resolved, got nil")
	}
}

func TestDeclaredCapabilityWithoutGrantHasNoCredential(t *testing.T) {
	inj, _ := newTestInjector(t, []Grant{{Capability: "github:issues:write", Ref: "github-issues"}})
	set, err := inj.Materialize(context.Background(), []string{"telemetry:read"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := set.Token(context.Background(), "telemetry:read"); !errors.Is(err, ErrNoCredentialForCapability) {
		t.Fatalf("Token error = %v, want ErrNoCredentialForCapability", err)
	}
}

func TestScopedTokenSourceMatchesTokenSourceShape(t *testing.T) {
	inj, _ := newTestInjector(t, []Grant{{Capability: "github:issues:write", Ref: "github-issues"}})
	set, err := inj.Materialize(context.Background(), []string{"github:issues:write"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// tokenSource is the shape providers.TokenSource declares
	// (Token(context.Context) (string, error)); ScopedTokenSource must
	// satisfy it structurally without this package importing providers.
	type tokenSource interface {
		Token(ctx context.Context) (string, error)
	}
	var ts tokenSource = set.For("github:issues:write")
	tok, err := ts.Token(context.Background())
	if err != nil || tok != "ghp_canaryTokenValue123456789" {
		t.Fatalf("ScopedTokenSource.Token = %q, %v", tok, err)
	}
}

// TestCanaryTokenNeverSurvivesScrub plants a token, materializes it through
// the seam, and proves that once its registrar has seen the value (as
// Materialize guarantees for every resolved credential), the token cannot
// appear anywhere in bytes a journal/telemetry writer would scrub before
// persisting — the acceptance criterion for issue #14.
func TestCanaryTokenNeverSurvivesScrub(t *testing.T) {
	inj, reg := newTestInjector(t, []Grant{{Capability: "github:issues:write", Ref: "github-issues"}})
	set, err := inj.Materialize(context.Background(), []string{"github:issues:write"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	tok, err := set.Token(context.Background(), "github:issues:write")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	// Simulate a journal event, a span, and an artifact — any bytes destined
	// for runs/ — that happen to embed the raw token (a stage logging a curl
	// command, an error message echoing an Authorization header, etc).
	simulatedWrites := [][]byte{
		[]byte(`{"type":"stage.finished","runner":{"cmd":"curl -H 'Authorization: token ` + tok + `'"}}`),
		[]byte("span: request failed for token=" + tok),
		[]byte("artifact bytes containing " + tok + " embedded mid-line"),
	}
	for _, raw := range simulatedWrites {
		scrubbed := reg.scrub(raw)
		if bytes.Contains(scrubbed, []byte(tok)) {
			t.Fatalf("token survived scrubbing in: %s", scrubbed)
		}
	}
}

func TestInjectorRejectsNilDependencies(t *testing.T) {
	resolver, _ := NewResolver(nil)
	if _, err := NewInjector(nil, nil, &spyRegistrar{}); err == nil {
		t.Fatal("NewInjector: want error for nil resolver")
	}
	if _, err := NewInjector(resolver, nil, nil); err == nil {
		t.Fatal("NewInjector: want error for nil registrar")
	}
}

func TestNewInjectorRejectsMalformedGrants(t *testing.T) {
	resolver, _ := NewResolver(nil)
	cases := [][]Grant{
		{{Capability: "", Ref: "x"}},
		{{Capability: "x", Ref: ""}},
		{{Capability: "x", Ref: "a"}, {Capability: "x", Ref: "b"}},
	}
	for _, grants := range cases {
		if _, err := NewInjector(resolver, grants, &spyRegistrar{}); err == nil {
			t.Fatalf("NewInjector(%+v): want error, got nil", grants)
		}
	}
}
