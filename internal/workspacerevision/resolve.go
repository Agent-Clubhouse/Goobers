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
	refs := append([]apiv1.RepoRef{base}, additional...)
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
	return *match, nil
}

func matches(identity apiv1.RepositoryIdentity, configured apiv1.RepoRef) bool {
	if identity.Provider != configured.Provider ||
		!strings.EqualFold(identity.Owner, configured.Owner) ||
		!strings.EqualFold(identity.Project, configured.Project) ||
		(!strings.EqualFold(identity.Name, configured.Name) &&
			(identity.Provider != apiv1.ProviderADO || identity.ID == "" || !strings.EqualFold(identity.ID, configured.Name))) {
		return false
	}
	raw := configured.BaseURL
	if raw == "" {
		if configured.Provider == apiv1.ProviderGitHub {
			raw = "https://github.com"
		} else if configured.Provider == apiv1.ProviderADO {
			raw = "https://dev.azure.com"
		}
	}
	configuredURL, err := url.Parse(raw)
	if err != nil || configuredURL.Hostname() == "" {
		return false
	}
	if identity.URL == "" {
		return configured.BaseURL == ""
	}
	selected, err := url.Parse(identity.URL)
	return err == nil && strings.EqualFold(selected.Scheme, configuredURL.Scheme) &&
		strings.EqualFold(selected.Host, configuredURL.Host)
}
