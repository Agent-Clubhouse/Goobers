package workspacerevision

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func validRevision() *apiv1.WorkspaceRevision {
	return &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo",
		},
		CommitSHA: strings.Repeat("a", 40),
		BaseRepository: &apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderGitHub, Owner: "org", Name: "base",
		},
	}
}

func TestAcceptCopiesAndIsIdempotent(t *testing.T) {
	candidate := validRevision()
	accepted, err := Accept(nil, candidate, true, true)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Repository.Name = "changed"
	if accepted.Repository.Name != "repo" {
		t.Fatal("accepted revision was not copied")
	}
	if _, err := Accept(accepted, validRevision(), true, true); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptRejectsAgenticAndConflicts(t *testing.T) {
	candidate := validRevision()
	if _, err := Accept(nil, candidate, false, true); err == nil || errorCode(err) != CodeUnauthorized {
		t.Fatalf("agentic result error = %v", err)
	}
	accepted, err := Accept(nil, candidate, true, true)
	if err != nil {
		t.Fatal(err)
	}
	other := validRevision()
	other.CommitSHA = strings.Repeat("b", 40)
	if _, err := Accept(accepted, other, true, true); err == nil || errorCode(err) != CodeConflict {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestAcceptFailedAndInvalidDoNotEstablish(t *testing.T) {
	candidate := validRevision()
	if got, err := Accept(nil, candidate, true, false); err != nil || got != nil {
		t.Fatalf("failed result established authority: %v %v", got, err)
	}
	candidate.CommitSHA = strings.ToUpper(candidate.CommitSHA)
	if got, err := Accept(nil, candidate, true, true); err == nil || got != nil || errorCode(err) != CodeInvalid {
		t.Fatalf("invalid result = %v %v", got, err)
	}
}

func TestResolveAuthorizesConfiguredRepositoryIdentity(t *testing.T) {
	revision := &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderGitea, URL: "https://git.example.test",
			Owner: "team", Name: "repo",
		},
		CommitSHA: strings.Repeat("a", 40),
	}
	configured := apiv1.RepoRef{
		Provider: apiv1.ProviderGitea, BaseURL: "https://git.example.test",
		Owner: "team", Name: "repo", Branch: "main",
	}
	got, err := Resolve(*revision, configured, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != configured {
		t.Fatalf("resolved ref = %+v, want %+v", got, configured)
	}
}

func TestResolveMatchesServiceRootURLs(t *testing.T) {
	tests := []struct {
		name       string
		provider   apiv1.Provider
		url        string
		configured apiv1.RepoRef
	}{
		{
			name:       "github",
			provider:   apiv1.ProviderGitHub,
			url:        "https://github.com",
			configured: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo"},
		},
		{
			name:       "ado",
			provider:   apiv1.ProviderADO,
			url:        "https://dev.azure.com",
			configured: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "project", Name: "repo"},
		},
		{
			name:       "self-hosted",
			provider:   apiv1.ProviderGitea,
			url:        "https://git.example.test",
			configured: apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: "https://git.example.test", Owner: "org", Name: "repo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			revision := validRevision()
			revision.BaseRepository = nil
			revision.Repository = apiv1.RepositoryIdentity{
				Provider: tt.provider,
				URL:      tt.url,
				Owner:    tt.configured.Owner,
				Project:  tt.configured.Project,
				Name:     tt.configured.Name,
			}
			if _, err := Resolve(*revision, tt.configured, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveRequiresADOProjectAndRepositoryName(t *testing.T) {
	revision := &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderADO, URL: "https://dev.azure.com",
			Owner: "acme", Project: "project", Name: "repo",
		},
		CommitSHA: strings.Repeat("a", 40),
	}
	configured := apiv1.RepoRef{
		Provider: apiv1.ProviderADO, Owner: "acme", Project: "project", Name: "repo",
	}
	if _, err := Resolve(*revision, configured, nil); err != nil {
		t.Fatal(err)
	}
	revision.Repository.Project = "other"
	if _, err := Resolve(*revision, configured, nil); errorCode(err) != CodeUnauthorized {
		t.Fatalf("project mismatch error = %v, want %s", err, CodeUnauthorized)
	}
}

func TestResolveRejectsUnverifiedADONativeRepositoryID(t *testing.T) {
	revision := &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderADO, URL: "https://dev.azure.com",
			Owner: "acme", Project: "project", Name: "repo", ID: "wrong-id",
		},
		CommitSHA: strings.Repeat("a", 40),
	}
	configured := apiv1.RepoRef{
		Provider: apiv1.ProviderADO, Owner: "acme", Project: "project", Name: "repo",
	}
	if _, err := Resolve(*revision, configured, nil); errorCode(err) != CodeUnauthorized {
		t.Fatalf("native repository ID mismatch error = %v, want %s", err, CodeUnauthorized)
	}
}

func TestResolveRejectsAmbiguousConfiguredPolicy(t *testing.T) {
	revision := validRevision()
	revision.BaseRepository = nil
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", Branch: "main"}
	additional := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", Branch: "release"}
	if _, err := Resolve(*revision, base, []apiv1.RepoRef{additional}); errorCode(err) != CodeUnauthorized {
		t.Fatalf("ambiguous match error = %v, want %s", err, CodeUnauthorized)
	}
}

func TestResolveAuthorizesOnlyConfiguredADONativeID(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	configured := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "project", Name: id, Branch: "main"}
	revision := apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderADO, URL: "https://dev.azure.com",
			Owner: "org", Project: "project", Name: "readable-name", ID: id,
		},
		CommitSHA: strings.Repeat("a", 40),
	}
	baseIdentity := revision.Repository
	revision.BaseRepository = &baseIdentity
	got, err := Resolve(revision, configured, nil)
	if err != nil || !reflect.DeepEqual(got, configured) {
		t.Fatalf("configured native ID: got=%+v err=%v", got, err)
	}
	revision.Repository.ID = "unverified-id"
	if _, err := Resolve(revision, configured, nil); errorCode(err) != CodeUnauthorized {
		t.Fatalf("incorrect ID authorized: %v", err)
	}
}

func errorCode(err error) string {
	var revisionErr *Error
	if errors.As(err, &revisionErr) {
		return revisionErr.Code
	}
	return ""
}
