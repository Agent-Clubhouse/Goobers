package workspacerevision

import (
	"net/url"
	"reflect"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Resolve authorizes a revision against configuration and returns configuration,
// never a reference reconstructed from stage output. RepoRef has no native ID:
// selected IDs are evidence only, not an alternate routing key. When ADO config
// names a repository by ID, that configured ID remains the routing authority.
// Callers must not overwrite the returned name, service, policy, or credentials
// with fields from the selected identity.
func Resolve(revision apiv1.WorkspaceRevision, base apiv1.RepoRef, additional []apiv1.RepoRef) (apiv1.RepoRef, error) {
	if err := revision.Validate(); err != nil {
		return apiv1.RepoRef{}, &Error{Code: CodeInvalid, Message: err.Error(), Cause: err}
	}
	if revision.BaseRepository != nil && !matches(*revision.BaseRepository, base) {
		return apiv1.RepoRef{}, &Error{Code: CodeUnauthorized, Message: "selected base repository does not match configured base"}
	}
	refs := make([]apiv1.RepoRef, 0, len(additional)+1)
	refs = append(refs, base)
	refs = append(refs, additional...)
	var resolved *apiv1.RepoRef
	for _, configured := range refs {
		if !matches(revision.Repository, configured) {
			continue
		}
		if resolved != nil && !reflect.DeepEqual(*resolved, configured) {
			return apiv1.RepoRef{}, &Error{Code: CodeUnauthorized, Message: "selected repository has ambiguous configured authority"}
		}
		ref := configured
		resolved = &ref
	}
	if resolved == nil {
		return apiv1.RepoRef{}, &Error{Code: CodeUnauthorized, Message: "selected repository is not declared by configuration"}
	}
	if resolved.Checkout != nil {
		checkout := *resolved.Checkout
		checkout.Sparse = append([]string(nil), checkout.Sparse...)
		resolved.Checkout = &checkout
	}
	return *resolved, nil
}

func matches(identity apiv1.RepositoryIdentity, configured apiv1.RepoRef) bool {
	if identity.Provider != configured.Provider ||
		!strings.EqualFold(identity.Owner, configured.Owner) ||
		!strings.EqualFold(identity.Project, configured.Project) {
		return false
	}
	nameMatches := strings.EqualFold(identity.Name, configured.Name)
	if !nameMatches && (identity.Provider != apiv1.ProviderADO || identity.ID == "" || !strings.EqualFold(identity.ID, configured.Name)) {
		return false
	}
	service, prefix, ok := configuredService(configured)
	if !ok {
		return false
	}
	if identity.URL == "" {
		return identity.Provider != apiv1.ProviderGitea && configured.BaseURL == ""
	}
	selected, err := url.Parse(identity.URL)
	if err != nil || !strings.EqualFold(selected.Scheme, service.Scheme) ||
		!strings.EqualFold(selected.Host, service.Host) {
		return false
	}
	path := strings.TrimSuffix(strings.TrimSuffix(selected.Path, "/"), ".git")
	switch identity.Provider {
	case apiv1.ProviderGitHub, apiv1.ProviderGitea:
		return strings.EqualFold(path, prefix+"/"+identity.Owner+"/"+identity.Name)
	case apiv1.ProviderADO:
		root := prefix + "/" + identity.Owner + "/" + identity.Project
		if strings.EqualFold(path, root+"/_git/"+identity.Name) {
			return true
		}
		return identity.ID != "" && (strings.EqualFold(path, root+"/_git/"+identity.ID) ||
			strings.EqualFold(path, root+"/_apis/git/repositories/"+identity.ID))
	default:
		return false
	}
}

func configuredService(ref apiv1.RepoRef) (*url.URL, string, bool) {
	raw := ref.BaseURL
	if raw == "" {
		switch ref.Provider {
		case apiv1.ProviderGitHub:
			raw = "https://github.com"
		case apiv1.ProviderADO:
			raw = "https://dev.azure.com"
		default:
			return nil, "", false
		}
	}
	service, err := url.Parse(raw)
	if err != nil || service.Hostname() == "" || (service.Scheme != "https" && service.Scheme != "http") ||
		service.User != nil || service.RawQuery != "" || service.ForceQuery || service.Fragment != "" {
		return nil, "", false
	}
	return service, strings.TrimSuffix(service.Path, "/"), true
}
