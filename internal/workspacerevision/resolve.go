package workspacerevision

import (
	"net/url"
	"reflect"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Resolve returns only a configured repository reference; stage data never
// supplies credentials, checkout policy, or routing authority.
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
	var match *apiv1.RepoRef
	for _, configured := range refs {
		if !matches(revision.Repository, configured) {
			continue
		}
		if match != nil && !reflect.DeepEqual(*match, configured) {
			return apiv1.RepoRef{}, &Error{Code: CodeUnauthorized, Message: "selected repository has ambiguous configured authority"}
		}
		copy := configured
		match = &copy
	}
	if match == nil {
		return apiv1.RepoRef{}, &Error{Code: CodeUnauthorized, Message: "selected repository is not declared by configuration"}
	}
	if match.Checkout != nil {
		checkout := *match.Checkout
		checkout.Sparse = append([]string(nil), checkout.Sparse...)
		match.Checkout = &checkout
	}
	return *match, nil
}

func matches(identity apiv1.RepositoryIdentity, configured apiv1.RepoRef) bool {
	if identity.Provider != configured.Provider ||
		!strings.EqualFold(identity.Owner, configured.Owner) ||
		!strings.EqualFold(identity.Project, configured.Project) {
		return false
	}
	if !strings.EqualFold(identity.Name, configured.Name) {
		return false
	}
	raw := configured.BaseURL
	switch {
	case raw == "" && configured.Provider == apiv1.ProviderGitHub:
		raw = "https://github.com"
	case raw == "" && configured.Provider == apiv1.ProviderADO:
		raw = "https://dev.azure.com"
	}
	configuredURL, err := url.Parse(raw)
	if err != nil || configuredURL.Hostname() == "" ||
		(configuredURL.Scheme != "http" && configuredURL.Scheme != "https") ||
		configuredURL.User != nil || configuredURL.RawQuery != "" || configuredURL.Fragment != "" {
		return false
	}
	if identity.URL == "" {
		return configured.Provider != apiv1.ProviderGitea && configured.BaseURL == ""
	}
	selected, err := url.Parse(identity.URL)
	if err != nil || !strings.EqualFold(selected.Scheme, configuredURL.Scheme) ||
		!strings.EqualFold(selected.Host, configuredURL.Host) ||
		!strings.EqualFold(strings.TrimSuffix(selected.Path, "/"), strings.TrimSuffix(configuredURL.Path, "/")) {
		return false
	}
	return true
}
