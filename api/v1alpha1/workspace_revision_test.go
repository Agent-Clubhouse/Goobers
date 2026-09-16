package v1alpha1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceRevisionValidation(t *testing.T) {
	valid := WorkspaceRevision{Repository: RepositoryIdentity{Provider: ProviderGitHub, Owner: "org", Name: "repo"}, CommitSHA: strings.Repeat("a", 40)}
	for _, tc := range []struct {
		name string
		sha  string
		ok   bool
	}{
		{"SHA1", strings.Repeat("a", 40), true},
		{"SHA256", strings.Repeat("b", 64), true},
		{"uppercase", strings.Repeat("A", 40), false},
		{"short", strings.Repeat("a", 39), false},
		{"long", strings.Repeat("a", 65), false},
		{"nonhex", strings.Repeat("g", 40), false},
		{"ref", "refs/heads/main", false},
		{"expression", strings.Repeat("a", 40) + "^0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revision := valid
			revision.CommitSHA = tc.sha
			if err := revision.Validate(); (err == nil) != tc.ok {
				t.Fatalf("valid=%v err=%v", tc.ok, err)
			}
		})
	}
	base := valid.Repository
	base.Owner = ""
	valid.BaseRepository = &base
	if valid.Validate() == nil {
		t.Fatal("malformed base identity accepted")
	}
	valid.BaseRepository = nil
	valid.BaseSHA = "main"
	if valid.Validate() == nil {
		t.Fatal("moving base SHA accepted")
	}
}

func TestRepositoryIdentityValidation(t *testing.T) {
	for _, identity := range []RepositoryIdentity{
		{Provider: "unknown", Owner: "org", Name: "repo"},
		{Provider: ProviderGitHub, Name: "repo"},
		{Provider: ProviderGitHub, Owner: "org", Name: "../repo"},
		{Provider: ProviderGitHub, Owner: " org", Name: "repo"},
		{Provider: ProviderGitHub, Owner: "org", Name: "repo", Project: "ado-only"},
		{Provider: ProviderADO, Owner: "org", Name: "repo"},
		{Provider: ProviderGitea, Owner: "org", Name: "repo"},
		{Provider: ProviderGitHub, Owner: "org", Name: "repo", URL: "https://token@github.com/org/repo"},
		{Provider: ProviderGitHub, Owner: "org", Name: "repo", URL: "https://github.com/org/repo?token=value"},
		{Provider: ProviderGitHub, Owner: "org", Name: "repo", URL: "file:///repo"},
	} {
		if identity.Validate() == nil {
			t.Errorf("accepted malformed identity %+v", identity)
		}
	}
}

func TestWorkspaceRevisionResultRoundTrip(t *testing.T) {
	base := RepositoryIdentity{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo", ID: "native-id", URL: "https://dev.azure.com/org/project/_git/repo"}
	result := ResultEnvelope{
		Status: ResultSuccess,
		WorkspaceRevision: &WorkspaceRevision{
			Repository: base, CommitSHA: strings.Repeat("a", 40), SourceRef: "refs/heads/topic",
			SourceID: "123", BaseRepository: &base, BaseSHA: strings.Repeat("b", 64),
		},
		Outputs:   map[string]interface{}{"workspaceBranch": "goobers/run"},
		Artifacts: []ArtifactPointer{{Path: "artifacts/workspace.delta", Digest: "sha256:" + strings.Repeat("a", 64)}},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ResultEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(result, decoded) {
		t.Fatalf("lossy roundtrip: %s err=%v", data, err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	var legacy ResultEnvelope
	if err := json.Unmarshal([]byte(`{"status":"success","outputs":{"workspaceBranch":"legacy"}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Validate(); err != nil || legacy.WorkspaceRevision != nil {
		t.Fatalf("legacy compatibility: %+v err=%v", legacy, err)
	}
	legacy.WorkspaceRevision = &WorkspaceRevision{}
	if legacy.Validate() == nil {
		t.Fatal("envelope accepted malformed workspace revision")
	}
}

func TestRepositoryIdentityCanonicalKey(t *testing.T) {
	r := RepositoryIdentity{Provider: ProviderADO, Owner: "ORG", Project: "Project", Name: "Repo", ID: "ID", URL: "https://ADO.EXAMPLE/org/project/_git/repo"}
	if got, want := r.CanonicalKey(), "ado|ado.example|project|org|repo|id"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
