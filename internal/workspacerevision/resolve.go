package workspacerevision

import (
	"context"
	"net/url"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// RepositoryLookup reads metadata using only a configured repository reference.
type RepositoryLookup func(context.Context, apiv1.RepoRef) (providers.RepositoryMetadata, error)

// Resolve authorizes against metadata read through configured routes. Neither
// the lookup nor the returned checkout policy is derived from stage data.
func Resolve(ctx context.Context, revision apiv1.WorkspaceRevision, base apiv1.RepoRef, additional []apiv1.RepoRef, lookup RepositoryLookup) (apiv1.RepoRef, error) {
	if err := revision.Validate(); err != nil {
		return apiv1.RepoRef{}, &Error{Code: CodeInvalid, Message: err.Error(), Cause: err}
	}
	if lookup == nil {
		return apiv1.RepoRef{}, unauthorized("trusted repository identity lookup is not configured")
	}
	if revision.BaseRepository != nil {
		ok, err := authorizeIdentity(ctx, *revision.BaseRepository, base, lookup)
		if err != nil {
			return apiv1.RepoRef{}, err
		}
		if !ok {
			return apiv1.RepoRef{}, unauthorized("selected base repository does not match configured base")
		}
	}
	var match *apiv1.RepoRef
	for _, configured := range append([]apiv1.RepoRef{base}, additional...) {
		ok, err := authorizeIdentity(ctx, revision.Repository, configured, lookup)
		if err != nil {
			return apiv1.RepoRef{}, err
		}
		if !ok {
			continue
		}
		if match != nil {
			return apiv1.RepoRef{}, unauthorized("selected repository has ambiguous configured authority")
		}
		match = configured.DeepCopy()
	}
	if match == nil {
		return apiv1.RepoRef{}, unauthorized("selected repository is not declared by configuration")
	}
	return *match, nil
}

func unauthorized(message string) *Error {
	return &Error{Code: CodeUnauthorized, Message: message}
}

func authorizeIdentity(ctx context.Context, candidate apiv1.RepositoryIdentity, configured apiv1.RepoRef, lookup RepositoryLookup) (bool, error) {
	if candidate.Provider != configured.Provider || !strings.EqualFold(candidate.Owner, configured.Owner) || !candidateServiceMatches(candidate, configured) {
		return false, nil
	}
	if !strings.EqualFold(candidate.Name, configured.Name) &&
		(configured.Provider != apiv1.ProviderADO || candidate.ID == "" || !strings.EqualFold(candidate.ID, configured.Name)) {
		return false, nil
	}
	metadata, err := lookup(ctx, *configured.DeepCopy())
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		code := CodeUnauthorized
		if providers.IsTransientError(err) {
			code = CodeAcquisition
		}
		return false, &Error{Code: code, Message: "trusted repository identity lookup failed: " + err.Error(), Cause: err}
	}
	if !trustedRouteMatches(metadata, configured) {
		return false, unauthorized("repository metadata does not match its configured route")
	}
	identity := metadata.Repository
	if !strings.EqualFold(candidate.Name, identity.Name) || !strings.EqualFold(candidate.Project, identity.Project) ||
		(candidate.ID != "" && !strings.EqualFold(candidate.ID, identity.ID)) {
		return false, nil
	}
	if candidate.URL == "" {
		return configured.BaseURL == "" && configured.Provider != apiv1.ProviderGitea, nil
	}
	return sameIdentityURL(candidate.URL, metadata.ServiceRoot, false) ||
		sameIdentityURL(candidate.URL, canonicalRepositoryURL(metadata), true), nil
}

func candidateServiceMatches(candidate apiv1.RepositoryIdentity, configured apiv1.RepoRef) bool {
	if candidate.URL == "" {
		return configured.BaseURL == "" && configured.Provider != apiv1.ProviderGitea
	}
	selected, err := normalizedIdentityURL(candidate.URL)
	if err != nil {
		return false
	}
	root, err := normalizedIdentityURL(serviceRoot(configured))
	return err == nil && (selected == root || strings.HasPrefix(selected, root+"/"))
}

func trustedRouteMatches(metadata providers.RepositoryMetadata, configured apiv1.RepoRef) bool {
	identity := metadata.Repository
	if identity.Provider != providers.ProviderKind(configured.Provider) ||
		!strings.EqualFold(identity.Owner, configured.Owner) || identity.Name == "" || identity.ID == "" ||
		!sameIdentityURL(metadata.ServiceRoot, serviceRoot(configured), false) {
		return false
	}
	nameMatches := strings.EqualFold(identity.Name, configured.Name)
	projectMatches := strings.EqualFold(identity.Project, configured.Project)
	if configured.Provider == apiv1.ProviderADO {
		nameMatches = nameMatches || strings.EqualFold(identity.ID, configured.Name)
		projectMatches = projectMatches || (metadata.ProjectID != "" && strings.EqualFold(metadata.ProjectID, configured.Project))
	}
	return nameMatches && projectMatches && sameIdentityURL(identity.URL, canonicalRepositoryURL(metadata), true)
}

func serviceRoot(configured apiv1.RepoRef) string {
	if configured.BaseURL != "" {
		return strings.TrimRight(configured.BaseURL, "/")
	}
	switch configured.Provider {
	case apiv1.ProviderGitHub:
		return "https://github.com"
	case apiv1.ProviderADO:
		return "https://dev.azure.com"
	default:
		return ""
	}
}

func canonicalRepositoryURL(metadata providers.RepositoryMetadata) string {
	repo := metadata.Repository
	root := strings.TrimRight(metadata.ServiceRoot, "/") + "/" + url.PathEscape(repo.Owner)
	if repo.Provider == providers.ProviderADO {
		root += "/" + url.PathEscape(repo.Project) + "/_git"
	}
	return root + "/" + url.PathEscape(repo.Name)
}

func sameIdentityURL(left, right string, repository bool) bool {
	a, err := normalizedIdentityURL(left)
	if err != nil {
		return false
	}
	b, err := normalizedIdentityURL(right)
	return err == nil && (a == b || (repository && a == b+".git"))
}

func normalizedIdentityURL(raw string) (string, error) {
	identity := apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "validation", Name: "validation", URL: raw}
	if raw == "" {
		return "", unauthorized("empty repository service URL")
	}
	if err := identity.Validate(); err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}
