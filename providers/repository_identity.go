package providers

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// RepositoryIdentity projects repository identity without adding checkout or
// credential authority. All provider identity fields survive the conversion.
func (r RepositoryRef) RepositoryIdentity() apiv1.RepositoryIdentity {
	return apiv1.RepositoryIdentity{
		Provider: apiv1.Provider(r.Provider),
		Owner:    r.Owner, Project: r.Project, Name: r.Name, ID: r.ID, URL: r.URL,
	}
}

// RepositoryRefFromIdentity reverses RepositoryIdentity losslessly.
func RepositoryRefFromIdentity(r apiv1.RepositoryIdentity) RepositoryRef {
	return RepositoryRef{
		Provider: ProviderKind(r.Provider),
		Owner:    r.Owner, Project: r.Project, Name: r.Name, ID: r.ID, URL: r.URL,
	}
}
