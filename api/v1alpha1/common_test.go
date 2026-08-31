package v1alpha1

import (
	"testing"
)

func TestBacklogIdentityFromRefGitHub(t *testing.T) {
	ref := BacklogRef{
		Provider: ProviderGitHub,
		Project:  "gim-home/brandiv.goobers",
	}
	id, err := BacklogIdentityFromRef(ref)
	if err != nil {
		t.Fatalf("BacklogIdentityFromRef: %v", err)
	}
	if id.Provider != ProviderGitHub {
		t.Errorf("Provider = %q, want %q", id.Provider, ProviderGitHub)
	}
	if id.Owner != "gim-home" {
		t.Errorf("Owner = %q, want %q", id.Owner, "gim-home")
	}
	if id.Name != "brandiv.goobers" {
		t.Errorf("Name = %q, want %q", id.Name, "brandiv.goobers")
	}
	if id.Project != "" {
		t.Errorf("Project = %q, want empty", id.Project)
	}
}

func TestBacklogIdentityFromRefGitea(t *testing.T) {
	ref := BacklogRef{
		Provider: ProviderGitea,
		BaseURL:  "https://gitea.example.com/",
		Project:  "acme/issues",
	}
	id, err := BacklogIdentityFromRef(ref)
	if err != nil {
		t.Fatalf("BacklogIdentityFromRef: %v", err)
	}
	if id.Provider != ProviderGitea {
		t.Errorf("Provider = %q, want %q", id.Provider, ProviderGitea)
	}
	if id.BaseURL != "https://gitea.example.com" {
		t.Errorf("BaseURL = %q, want normalized (no trailing slash)", id.BaseURL)
	}
	if id.Owner != "acme" || id.Name != "issues" {
		t.Errorf("Owner/Name = %q/%q, want acme/issues", id.Owner, id.Name)
	}
}

func TestBacklogIdentityFromRefADO(t *testing.T) {
	ref := BacklogRef{
		Provider: ProviderADO,
		Project:  "TeamProject",
	}
	id, err := BacklogIdentityFromRef(ref)
	if err != nil {
		t.Fatalf("BacklogIdentityFromRef: %v", err)
	}
	if id.Provider != ProviderADO {
		t.Errorf("Provider = %q, want %q", id.Provider, ProviderADO)
	}
	if id.Project != "TeamProject" {
		t.Errorf("Project = %q, want %q", id.Project, "TeamProject")
	}
}

func TestBacklogIdentityFromRefRejectsMalformedGitHubProject(t *testing.T) {
	// A GitHub/Gitea backlog is addressed as exactly "owner/name". Anything
	// else must be refused rather than split arbitrarily: a lenient split would
	// let two spellings of the same configuration produce two different
	// ownership keys, which is precisely the collision the canonical identity
	// exists to prevent.
	for _, project := range []string{"", "noSlash", "/leading", "trailing/", "too/many/slashes", "a//b"} {
		ref := BacklogRef{Provider: ProviderGitHub, Project: project}
		if _, err := BacklogIdentityFromRef(ref); err == nil {
			t.Errorf("BacklogIdentityFromRef(%q) should fail", project)
		}
	}
}

func TestBacklogIdentityString(t *testing.T) {
	id := BacklogIdentity{
		Provider: ProviderGitHub,
		Owner:    "gim-home",
		Name:     "brandiv.goobers",
	}
	s := id.String()
	if s == "" {
		t.Fatal("String() is empty")
	}
	// Should be deterministic.
	if id.String() != s {
		t.Fatalf("String() is not deterministic: %q vs %q", id.String(), s)
	}
}

func TestBacklogIdentityEqual(t *testing.T) {
	a := BacklogIdentity{Provider: ProviderGitHub, Owner: "acme", Name: "issues"}
	b := BacklogIdentity{Provider: ProviderGitHub, Owner: "acme", Name: "issues"}
	if !a.Equal(b) {
		t.Fatal("identical identities should be equal")
	}

	c := BacklogIdentity{Provider: ProviderGitHub, Owner: "acme", Name: "other"}
	if a.Equal(c) {
		t.Fatal("different identities should not be equal")
	}
}

func TestBacklogIdentityDifferentBacklogsSameExternalIDIndependent(t *testing.T) {
	// Equal external IDs in different backlogs must produce different identity strings.
	a := BacklogIdentity{Provider: ProviderGitHub, Owner: "org-a", Name: "repo-a"}
	b := BacklogIdentity{Provider: ProviderGitHub, Owner: "org-b", Name: "repo-b"}
	if a.String() == b.String() {
		t.Fatalf("different backlogs must produce different keys: %q", a.String())
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"https://GITEA.Example.Com/", "https://gitea.example.com"},
		{"https://gitea.example.com", "https://gitea.example.com"},
		{"HTTPS://HOST///", "https://host"},
	}
	for _, tt := range tests {
		got := normalizeBaseURL(tt.input)
		if got != tt.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
