package bootstrap

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestBacklogProviderForGitHub(t *testing.T) {
	p, repo, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/web"}, "tok", nil, nil)
	if err != nil {
		t.Fatalf("BacklogProviderFor: %v", err)
	}
	if p == nil {
		t.Fatal("nil provider")
	}
	if repo.Provider != providers.ProviderGitHub || repo.Owner != "acme" || repo.Name != "web" {
		t.Errorf("repo = %+v, want github acme/web", repo)
	}
}

func TestBacklogProviderForADO(t *testing.T) {
	const token = "ado-token-value"
	reg := journal.NewRegistryScrubber()
	_, repo, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "myorg/myproject"}, token, reg, nil)
	if err != nil {
		t.Fatalf("BacklogProviderFor: %v", err)
	}
	if repo.Provider != providers.ProviderADO || repo.Project != "myproject" {
		t.Errorf("repo = %+v, want ado myproject", repo)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("goobers:" + token))
	if got := string(reg.Scrub([]byte(encoded))); got != journal.Redacted {
		t.Fatalf("encoded ADO credential was not registered: %q", got)
	}
}

func TestBacklogProviderForErrors(t *testing.T) {
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "noslash"}, "t", nil, nil); err == nil {
		t.Error("expected error for malformed github project")
	}
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{Provider: "gitlab", Project: "a/b"}, "t", nil, nil); err == nil {
		t.Error("expected error for unsupported provider")
	}
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "a/b"}, "credential", nil, nil); err == nil {
		t.Error("expected error for credentialed ADO provider without registrar")
	}
}

type bootstrapRateLimitObserver struct {
	events []providers.RateLimitEvent
}

func (o *bootstrapRateLimitObserver) ObserveRateLimit(_ context.Context, ev providers.RateLimitEvent) {
	o.events = append(o.events, ev)
}

type cancelingRateLimitClient struct {
	cancel context.CancelFunc
}

func (c cancelingRateLimitClient) Do(*http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"60"}},
		Body:       io.NopCloser(strings.NewReader("rate limited")),
	}, nil
}

func TestBacklogProviderForWiresRateLimitObserver(t *testing.T) {
	observer := &bootstrapRateLimitObserver{}
	reg := journal.NewRegistryScrubber()
	tests := []struct {
		name      string
		backlog   apiv1.BacklogRef
		registrar providers.SecretRegistrar
		exercise  func(context.Context, context.CancelFunc, providers.BacklogProvider)
	}{
		{
			name:    "github",
			backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/web"},
			exercise: func(ctx context.Context, cancel context.CancelFunc, provider providers.BacklogProvider) {
				p := provider.(*providers.GitHubProvider)
				p.Client = cancelingRateLimitClient{cancel: cancel}
				_, _ = p.GetWorkItem(ctx, providers.RepositoryRef{Owner: "acme", Name: "web"}, "42")
			},
		},
		{
			name:      "ado",
			backlog:   apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "org/project"},
			registrar: reg,
			exercise: func(ctx context.Context, cancel context.CancelFunc, provider providers.BacklogProvider) {
				p := provider.(*providers.ADOProvider)
				p.Client = cancelingRateLimitClient{cancel: cancel}
				_, _ = p.GetWorkItem(ctx, providers.RepositoryRef{Project: "project"}, "42")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(observer.events)
			provider, _, err := BacklogProviderFor(test.backlog, "token", test.registrar, observer)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			test.exercise(ctx, cancel, provider)
			if len(observer.events) != before+1 {
				t.Fatalf("rate-limit events = %#v, want event from %s provider", observer.events, test.name)
			}
		})
	}
}

func TestBacklogWorkflows(t *testing.T) {
	loaded, err := LoadAndRegister(fixtureRoot, "")
	if err != nil {
		t.Fatalf("LoadAndRegister: %v", err)
	}
	g := loaded.Gaggles[0]
	names := loaded.BacklogWorkflows(g.Name)
	if len(names) == 0 {
		t.Fatalf("expected at least one backlog-triggered workflow for gaggle %q", g.Name)
	}
	// And none for an unknown gaggle.
	if got := loaded.BacklogWorkflows("ghost"); len(got) != 0 {
		t.Errorf("unknown gaggle should have no workflows, got %v", got)
	}
}
