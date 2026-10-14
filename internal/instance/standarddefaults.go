package instance

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// The placeholder gaggle and repository identity `goobers init
// --template=standard` scaffolds when no --repo is given.
const (
	StandardPlaceholderGaggle  = "example"
	StandardPlaceholderOwner   = "your-org"
	StandardPlaceholderProject = "your-project"
	StandardPlaceholderRepo    = "your-repo"
)

// StandardInitDefaults returns the GuidedOptions `goobers init
// --template=standard --provider=<provider>` starts from before any flag
// overrides them: the placeholder gaggle and repository identity (an Azure
// DevOps repository also gets a placeholder project) and the provider's
// credential defaults.
//
// GitHub names one token variable per capability class. Azure DevOps names
// none, because azure-cli and managed-identity auth read no variable, unless
// repoAuthKind is pat, whose one variable is GOOBERS_ADO_TOKEN. repoAuthKind is
// carried through as given; an empty kind on Azure DevOps becomes azure-cli
// when the options are seeded.
//
// The provider compile-matrix gate (test/providermatrix) seeds its scaffold
// columns from this as well, so the scaffold it validates is the one init
// writes rather than a hand-copied approximation.
func StandardInitDefaults(provider, repoAuthKind string) GuidedOptions {
	opts := GuidedOptions{
		GaggleName:   StandardPlaceholderGaggle,
		RepoProvider: provider,
		RepoOwner:    StandardPlaceholderOwner,
		RepoName:     StandardPlaceholderRepo,
		RepoAuthKind: repoAuthKind,
	}
	if provider != string(apiv1.ProviderADO) {
		opts.RepoTokenEnv = "GOOBERS_GITHUB_TOKEN"
		opts.WorkTrackingTokenEnv = "GOOBERS_GITHUB_ISSUES_TOKEN"
		opts.PullRequestTokenEnv = "GOOBERS_GITHUB_PR_TOKEN"
		opts.RepoPushTokenEnv = "GOOBERS_GITHUB_PUSH_TOKEN"
		return opts
	}
	opts.RepoProject = StandardPlaceholderProject
	if strings.TrimSpace(repoAuthKind) == ADOAuthPAT {
		opts.RepoTokenEnv = "GOOBERS_ADO_TOKEN"
	}
	return opts
}
