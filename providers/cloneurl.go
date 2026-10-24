package providers

import (
	"fmt"
	"net/url"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// CloneURL derives the git remote URL worktree.Manager clones from
// a RepoRef.
func CloneURL(ref apiv1.RepoRef) (string, error) {
	switch ref.Provider {
	case apiv1.ProviderGitHub:
		return fmt.Sprintf("https://github.com/%s/%s.git", ref.Owner, ref.Name), nil
	case apiv1.ProviderADO:
		organization := ref.Owner
		project := ref.Project
		if project == "" {
			organization, project, _ = strings.Cut(ref.Owner, "/")
		}
		if organization == "" || project == "" {
			return "", fmt.Errorf("runner: ADO repo owner and project are required")
		}
		return fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s",
			url.PathEscape(organization), url.PathEscape(project), url.PathEscape(ref.Name)), nil
	case apiv1.ProviderGitea:
		// BaseURL here is the forge host root (e.g. https://gitea.example.com),
		// NOT the /api/v1 REST root: git clone/fetch speak smart-HTTP against
		// <root>/<owner>/<name>.git. This URL keys the MGV-11 git-auth resolver.
		base := strings.TrimSuffix(strings.TrimSpace(ref.BaseURL), "/")
		if base == "" {
			return "", fmt.Errorf("runner: gitea repo requires baseUrl")
		}
		return fmt.Sprintf("%s/%s/%s.git", base, ref.Owner, ref.Name), nil
	default:
		return "", fmt.Errorf("runner: unsupported repo provider %q", ref.Provider)
	}
}
