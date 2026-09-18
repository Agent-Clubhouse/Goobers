package v1alpha1

import (
	"strings"
	"testing"
)

func TestWorkspaceRevisionValidatesFullLowercaseObjectIDs(t *testing.T) {
	for _, size := range []int{40, 64} {
		revision := WorkspaceRevision{
			Repository: RepositoryIdentity{Provider: ProviderGitHub, Owner: "org", Name: "repo"},
			CommitSHA:  strings.Repeat("a", size),
		}
		if err := revision.Validate(); err != nil {
			t.Fatalf("%d-character SHA: %v", size, err)
		}
	}
	for _, sha := range []string{strings.Repeat("a", 39), strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		if err := ValidateCommitSHA(sha); err == nil {
			t.Errorf("accepted malformed SHA %q", sha)
		}
	}
}

func TestWorkspaceRevisionDeepCopyCopiesNestedIdentity(t *testing.T) {
	revision := &WorkspaceRevision{
		Repository: RepositoryIdentity{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"},
		CommitSHA:  strings.Repeat("a", 40),
		BaseRepository: &RepositoryIdentity{
			Provider: ProviderADO, Owner: "org", Project: "project", Name: "base",
		},
	}
	copy := revision.DeepCopy()
	copy.BaseRepository.Name = "changed"
	if revision.BaseRepository.Name != "base" {
		t.Fatal("deep copy shares nested base repository")
	}
}
