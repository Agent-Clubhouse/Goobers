package workspacerevision

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func lookupFixture(_ context.Context, configured apiv1.RepoRef) (providers.RepositoryMetadata, error) {
	metadata := providers.RepositoryMetadata{ServiceRoot: serviceRoot(configured), Repository: providers.RepositoryRef{
		Provider: providers.ProviderKind(configured.Provider), Owner: configured.Owner,
		Project: configured.Project, Name: configured.Name, ID: "17",
	}}
	if configured.Name == "12345678-1234-1234-1234-123456789abc" {
		metadata.Repository.Name, metadata.Repository.ID = "readable-name", configured.Name
	}
	metadata.Repository.URL = canonicalRepositoryURL(metadata)
	return metadata, nil
}

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
	accepted, err := Accept(nil, candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Repository.Name = "changed"
	if accepted.Repository.Name != "repo" {
		t.Fatal("accepted revision was not copied")
	}
	if _, err := Accept(accepted, validRevision()); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptRejectsConflicts(t *testing.T) {
	candidate := validRevision()
	accepted, err := Accept(nil, candidate)
	if err != nil {
		t.Fatal(err)
	}
	other := validRevision()
	other.CommitSHA = strings.Repeat("b", 40)
	if _, err := Accept(accepted, other); err == nil || errorCode(err) != CodeConflict {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestAcceptAbsentAndInvalidDoNotEstablish(t *testing.T) {
	candidate := validRevision()
	if got, err := Accept(nil, nil); err != nil || got != nil {
		t.Fatalf("absent result established authority: %v %v", got, err)
	}
	candidate.CommitSHA = strings.ToUpper(candidate.CommitSHA)
	if got, err := Accept(nil, candidate); err == nil || got != nil || errorCode(err) != CodeInvalid {
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
	got, err := Resolve(context.Background(), *revision, configured, nil, lookupFixture)
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
			if _, err := Resolve(context.Background(), *revision, tt.configured, nil, lookupFixture); err != nil {
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
	if _, err := Resolve(context.Background(), *revision, configured, nil, lookupFixture); err != nil {
		t.Fatal(err)
	}
	revision.Repository.Project = "other"
	if _, err := Resolve(context.Background(), *revision, configured, nil, lookupFixture); errorCode(err) != CodeUnauthorized {
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
	if _, err := Resolve(context.Background(), *revision, configured, nil, lookupFixture); errorCode(err) != CodeUnauthorized {
		t.Fatalf("native repository ID mismatch error = %v, want %s", err, CodeUnauthorized)
	}
}

func TestResolveRejectsAmbiguousConfiguredPolicy(t *testing.T) {
	revision := validRevision()
	revision.BaseRepository = nil
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", Branch: "main"}
	additional := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", Branch: "release"}
	if _, err := Resolve(context.Background(), *revision, base, []apiv1.RepoRef{additional}, lookupFixture); errorCode(err) != CodeUnauthorized {
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
	got, err := Resolve(context.Background(), revision, configured, nil, lookupFixture)
	if err != nil || !reflect.DeepEqual(got, configured) {
		t.Fatalf("configured native ID: got=%+v err=%v", got, err)
	}
	revision.Repository.ID = "unverified-id"
	if _, err := Resolve(context.Background(), revision, configured, nil, lookupFixture); errorCode(err) != CodeUnauthorized {
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
