package bootstrap

import (
	"encoding/base64"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestBacklogProviderForGitHub(t *testing.T) {
	p, repo, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/web"}, "tok", nil)
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
	_, repo, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "myorg/myproject"}, token, reg)
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
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "noslash"}, "t", nil); err == nil {
		t.Error("expected error for malformed github project")
	}
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{Provider: "gitlab", Project: "a/b"}, "t", nil); err == nil {
		t.Error("expected error for unsupported provider")
	}
	if _, _, err := BacklogProviderFor(apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "a/b"}, "credential", nil); err == nil {
		t.Error("expected error for credentialed ADO provider without registrar")
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
