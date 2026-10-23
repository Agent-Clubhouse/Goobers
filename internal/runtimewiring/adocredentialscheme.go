package runtimewiring

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

// AdoCredentialSchemes maps each credential key granted to a gaggle whose
// repository is on Azure DevOps to the authorization scheme its token is sent
// in (#5957). One stage's capabilities can be backed by different sources, so
// one scheme per resolution is not enough:
//
//   - a key backed by an Azure DevOps repository's own credential keeps that
//     repository's scheme (adoauth.AuthScheme): "bearer" for azure-cli,
//     workload-identity and managed-identity, "basic" for pat;
//   - a key backed by a static credentials: entry, or by a daemonIdentity of
//     kind "pat", is a personal access token, sent as "basic"
//     whatever the repository's own kind. Before #5957 such a token on an
//     Entra gaggle was sent as Bearer and Azure DevOps answered 401.
//
// Every other source (a GitHub repository, a GitHub App) states no Azure
// DevOps scheme, so its key is absent and the repository's scheme still
// applies to it as before. A static daemon identity that is a GitHub PAT
// (topology (b)) only reaches GitHub consumers, which never read a scheme. The map is nil for a gaggle whose
// repository is not on Azure DevOps: no scheme is stated there at all.
func AdoCredentialSchemes(cfg *instance.Config, project apiv1.RepoRef, grants []credentials.Grant) map[string]string {
	if _, ok := AdoRepoForGaggle(cfg, project); !ok {
		return nil
	}
	refSchemes := adoCredentialRefSchemes(cfg)
	schemes := make(map[string]string, len(grants))
	for _, grant := range grants {
		if scheme := refSchemes[grant.Ref]; scheme != "" {
			schemes[grant.Capability] = scheme
		}
	}
	return schemes
}

// adoCredentialRefSchemes is the scheme of each resolver ref that states one:
// every Azure DevOps repository's binding ref (buildRoleCredentials' naming,
// "<org>/<project>/<name>"), every static credentials: entry's ref and the
// ref of a daemon identity of kind "pat".
func adoCredentialRefSchemes(cfg *instance.Config) map[string]string {
	schemes := make(map[string]string, len(cfg.Repos)+len(cfg.Credentials))
	for _, repo := range cfg.Repos {
		if repo.Provider != string(apiv1.ProviderADO) {
			continue
		}
		owner := repo.Owner
		if repo.Project != "" {
			owner += "/" + repo.Project
		}
		if scheme := adoauth.AuthScheme(repo); scheme != "" {
			schemes[owner+"/"+repo.Name] = scheme
		}
	}
	if cfg.DaemonIdentity != nil && cfg.DaemonIdentity.Kind == instance.GitHubAuthPAT {
		schemes[DaemonIdentityRefName] = adoauth.SchemeBasic
	}
	for _, grant := range cfg.Credentials {
		if grant.GitHubApp != nil {
			continue
		}
		key, err := CredentialGrantStorageKey(grant)
		if err != nil {
			continue
		}
		schemes[CredentialRefName(key)] = adoauth.SchemeBasic
	}
	return schemes
}
