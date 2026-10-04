package interactiveaccess

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

type testRegistrar struct{ values []string }

func (r *testRegistrar) Register(value []byte) { r.values = append(r.values, string(value)) }

func testGaggle() apiv1.Gaggle {
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	return apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{Project: repo, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/issues"}, InteractiveAccess: &apiv1.InteractiveAccessPolicy{
		Humans:      apiv1.InteractiveHumanGrants{Viewers: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Group: "readers"}}, Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "alice"}}},
		Actions:     []apiv1.InteractiveAction{"backlog.read", "backlog.edit", "repository.read", "source.proposeChange", "run.intervene"},
		Credentials: apiv1.InteractiveCredentialBindings{Backlog: "issues", Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: repositoryIdentity(repo), CredentialRef: "code"}}},
	}}}
}
func testPrincipal() httpapi.Principal {
	return httpapi.Principal{Subject: "alice", Issuer: "https://identity.example", Roles: []httpapi.Role{httpapi.RoleOperate}}
}
func testSources() []instance.InteractiveCredential {
	return []instance.InteractiveCredential{{Name: "issues", Provider: "github", Owner: "acme", Repository: "issues", Token: instance.TokenRef{Env: "HUMAN_ISSUES_TOKEN"}}, {Name: "code", Provider: "github", Owner: "acme", Repository: "web", Token: instance.TokenRef{Env: "HUMAN_CODE_TOKEN"}}}
}
func testService(t *testing.T, g apiv1.Gaggle, sources []instance.InteractiveCredential) (*Service, *testRegistrar) {
	t.Helper()
	r := &testRegistrar{}
	s, err := New([]apiv1.Gaggle{g}, sources, Dependencies{Registrar: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestInteractiveMembershipIsExplicitIncludingAdmin(t *testing.T) {
	g := testGaggle()
	s, _ := testService(t, g, testSources())
	for _, tc := range []struct {
		name    string
		p       httpapi.Principal
		action  apiv1.InteractiveAction
		allowed bool
	}{
		{"operator", testPrincipal(), "backlog.edit", true},
		{"unlisted admin", httpapi.Principal{Subject: "root", Issuer: "https://identity.example", Roles: []httpapi.Role{httpapi.RoleAdmin}}, "backlog.edit", false},
		{"listed admin", httpapi.Principal{Subject: "alice", Issuer: "https://identity.example", Roles: []httpapi.Role{httpapi.RoleAdmin}}, "backlog.edit", true},
		{"group viewer", httpapi.Principal{Subject: "bob", Issuer: "https://identity.example", Groups: []string{"readers"}, Roles: []httpapi.Role{httpapi.RoleView}}, "backlog.read", true},
		{"viewer cannot edit", httpapi.Principal{Subject: "bob", Issuer: "https://identity.example", Groups: []string{"readers"}, Roles: []httpapi.Role{httpapi.RoleAdmin}}, "backlog.edit", false},
		{"instance floor", httpapi.Principal{Subject: "alice", Issuer: "https://identity.example", Roles: []httpapi.Role{httpapi.RoleView}}, "backlog.edit", false},
		{"issuer mismatch", httpapi.Principal{Subject: "alice", Issuer: "https://other.example", Roles: []httpapi.Role{httpapi.RoleAdmin}}, "backlog.edit", false},
		{"anonymous", httpapi.Principal{}, "backlog.read", false},
		{"stage principal", httpapi.Principal{Subject: "alice", Issuer: httpapi.PodPrincipalIssuer, Roles: []httpapi.Role{httpapi.RoleAdmin}, Groups: []string{"readers"}}, "backlog.edit", false},
		{"action omitted", testPrincipal(), "pr.repair", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Authorize(tc.p, "web", tc.action)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
}

func TestInteractivePermissionReasonsAndImmutablePolicy(t *testing.T) {
	g := testGaggle()
	sources := testSources()
	s, _ := testService(t, g, sources)
	g.Spec.InteractiveAccess.Humans.Operators[0].Subject = "mallory"
	sources[0].Owner = "elsewhere"
	result, err := s.InteractiveCapabilities(context.Background(), testPrincipal(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Operator || result.SourceWriteMode != "pull-request" {
		t.Fatalf("permissions=%+v", result)
	}
	for _, action := range result.Actions {
		if action.Available {
			t.Fatal("unimplemented operation advertised")
		}
		if action.Action == "backlog.edit" && (!action.Authorized || !action.CredentialConfigured || action.ReasonCode != "operation_not_implemented") {
			t.Fatalf("action=%+v", action)
		}
	}
	missing := testGaggle()
	missing.Spec.InteractiveAccess.Credentials.Backlog = "absent"
	if err := s.Apply([]apiv1.Gaggle{missing}, nil); err != nil {
		t.Fatal(err)
	}
	result, err = s.InteractiveCapabilities(context.Background(), testPrincipal(), "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range result.Actions {
		if action.Action == "backlog.read" && action.ReasonCode != "credential_not_configured" {
			t.Fatalf("action=%+v", action)
		}
	}
	missing.Spec.InteractiveAccess = nil
	if err := s.Apply([]apiv1.Gaggle{missing}, nil); err != nil {
		t.Fatal(err)
	}
	result, err = s.InteractiveCapabilities(context.Background(), testPrincipal(), "web")
	if err != nil || result.PolicyConfigured || result.Viewer {
		t.Fatalf("permissions=%+v error=%v", result, err)
	}
	for _, action := range result.Actions {
		if action.ReasonCode != "policy_missing" {
			t.Fatalf("action=%+v", action)
		}
	}
	if err := s.Authorize(testPrincipal(), "web", "run.intervene"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestInteractiveCredentialExactSelectionAndRedaction(t *testing.T) {
	t.Setenv("HUMAN_ISSUES_TOKEN", "human-issues-secret")
	t.Setenv("HUMAN_CODE_TOKEN", "human-code-secret")
	g := testGaggle()
	s, registrar := testService(t, g, testSources())
	ctx := context.Background()
	p := testPrincipal()
	for _, tc := range []struct {
		action apiv1.InteractiveAction
		target Target
		want   string
	}{{"backlog.read", Target{Kind: "backlog"}, "human-issues-secret"}, {"repository.read", Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}, "human-code-secret"}} {
		if err := s.WithCredential(ctx, p, "web", tc.action, tc.target, func(_ context.Context, value Credential) error {
			if value.Value != tc.want || value.Scheme != "bearer" {
				t.Fatal("wrong credential selected")
			}
			if !slices.Contains(registrar.values, tc.want) {
				t.Fatal("credential exposed before redaction registration")
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", value, value), tc.want) {
				t.Fatal("credential formatting leaked secret")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	use := func(context.Context, Credential) error { called = true; return nil }
	for _, target := range []Target{{Kind: "repository", Repository: apiv1.InteractiveRepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "web"}}, {Kind: "repository", Repository: apiv1.InteractiveRepositoryIdentity{Provider: apiv1.ProviderADO, Owner: "acme", Project: "x", Name: "web"}}, {Kind: "backlog", Repository: repositoryIdentity(g.Spec.Project)}} {
		if err := s.WithCredential(ctx, p, "web", "repository.read", target, use); err == nil {
			t.Fatal("accepted foreign target")
		}
	}
	g.Spec.InteractiveAccess.Credentials.Backlog = "code"
	if err := s.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WithCredential(ctx, p, "web", "backlog.read", Target{Kind: "backlog"}, use); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("cross-target source=%v", err)
	}
	if called {
		t.Fatal("denied operation reached credential callback")
	}
}

func TestInteractiveADOProjectBacklogAndIndependentGitHubBacklog(t *testing.T) {
	t.Setenv("HUMAN_ADO_TOKEN", "ado-secret")
	t.Setenv("HUMAN_ISSUES_TOKEN", "gh-secret")
	g := testGaggle()
	g.Spec.Project = apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "Code Project", Name: "web"}
	g.Spec.Backlog = apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "Work Project"}
	g.Spec.InteractiveAccess.Credentials.Backlog = "ado-work"
	sources := append(testSources(), instance.InteractiveCredential{Name: "ado-work", Provider: "ado", Owner: "org", Project: "Work Project", Token: instance.TokenRef{Env: "HUMAN_ADO_TOKEN"}})
	s, r := testService(t, g, sources)
	err := s.WithCredential(context.Background(), testPrincipal(), "web", "backlog.read", Target{Kind: "backlog"}, func(_ context.Context, value Credential) error {
		if value.Value != "ado-secret" || value.Scheme != "basic" || !slices.Contains(r.values, value.Value) {
			t.Fatal("ADO project credential was not scoped and scrubbed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g.Spec.Backlog = apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/issues"}
	g.Spec.InteractiveAccess.Credentials.Backlog = "issues"
	if err := s.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WithCredential(context.Background(), testPrincipal(), "web", "backlog.read", Target{Kind: "backlog"}, func(_ context.Context, value Credential) error {
		if value.Value != "gh-secret" {
			t.Fatal("code credential leaked to backlog")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveReloadWaitsForEffectThenRevokes(t *testing.T) {
	t.Setenv("HUMAN_ISSUES_TOKEN", "secret")
	g := testGaggle()
	s, _ := testService(t, g, testSources())
	entered := make(chan struct{})
	release := make(chan struct{})
	effect := make(chan error, 1)
	go func() {
		effect <- s.WithCredential(context.Background(), testPrincipal(), "web", "backlog.edit", Target{Kind: "backlog"}, func(context.Context, Credential) error { close(entered); <-release; return nil })
	}()
	<-entered
	g.Spec.InteractiveAccess = nil
	applying := make(chan struct{})
	published := make(chan struct{})
	applied := make(chan error, 1)
	go func() {
		close(applying)
		applied <- s.Apply([]apiv1.Gaggle{g}, func() error { close(published); return nil })
	}()
	<-applying
	select {
	case <-published:
		t.Fatal("reload published during old-policy effect")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-effect; err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(testPrincipal(), "web", "backlog.edit"); !errors.Is(err, ErrDenied) {
		t.Fatalf("revocation=%v", err)
	}
	// Failed catalog publication preserves the prior applied policy.
	s, _ = testService(t, testGaggle(), testSources())
	want := errors.New("publish failed")
	if err := s.Apply([]apiv1.Gaggle{g}, func() error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if err := s.Authorize(testPrincipal(), "web", "backlog.edit"); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveNonProviderAcceptanceRequiresPolicy(t *testing.T) {
	s, _ := testService(t, testGaggle(), testSources())
	called := 0
	accept := func(context.Context) error { called++; return nil }
	if err := s.WithAuthorization(context.Background(), testPrincipal(), "web", "run.intervene", accept); err != nil {
		t.Fatal(err)
	}
	for _, action := range []apiv1.InteractiveAction{"backlog.edit", "session.create", "unknown"} {
		if err := s.WithAuthorization(context.Background(), testPrincipal(), "web", action, accept); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s accepted: %v", action, err)
		}
	}
	if err := s.WithAuthorization(context.Background(), testPrincipal(), "other", "run.intervene", accept); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("accepted=%d", called)
	}
}
