package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// adoRepositoryIDReader reads the configured repository's own project and
// repository GUIDs; *providers.ADOProvider implements it.
type adoRepositoryIDReader interface {
	RepositoryIDs(context.Context, providers.RepositoryRef) (providers.ADORepositoryIDs, error)
}

// adoPullRequestURLByIDMatch is the last word on a targeted pull request
// whose URL failed pullRequestURLMatchesRepository. That matcher accepts the
// browser form and the API form that names the repository by name (ADO-N39),
// but ADO's raw API URL usually names the project and repository by GUID,
// which nothing in the URL ties to the configured repository (#5960). For
// that shape alone, the GUIDs are checked against the configured
// repository's own ids, read from the same provider: a match is accepted, a
// mismatch is the PR genuinely living elsewhere. When the ids cannot be read
// the answer stays a refusal, but a distinct one that says why and points at
// the web URL, instead of claiming the PR is outside the repository. Every
// other URL keeps the generic "outside configured repository" error.
func adoPullRequestURLByIDMatch(
	ctx context.Context,
	provider any,
	repo providers.RepositoryRef,
	resolvedRepo apiv1.RepoRef,
	rawURL string,
	number int,
	repoDisplay string,
) error {
	outside := fmt.Errorf("pull request #%d resolves outside configured repository %s", number, repoDisplay)
	org, project, repository, n, ok := providers.ParseADOPullRequestAPIURL(rawURL)
	if !ok || resolvedRepo.Provider != apiv1.ProviderADO || n != number || !strings.EqualFold(org, resolvedRepo.Owner) ||
		!(providers.IsADOGUID(project) || providers.IsADOGUID(repository)) {
		return outside
	}
	reader, ok := provider.(adoRepositoryIDReader)
	if !ok {
		return adoPullRequestURLUnverifiable(resolvedRepo, rawURL, number, repoDisplay, fmt.Errorf("provider %q cannot read repository ids", repo.Provider))
	}
	ids, err := reader.RepositoryIDs(ctx, repo)
	if err != nil {
		return adoPullRequestURLUnverifiable(resolvedRepo, rawURL, number, repoDisplay, err)
	}
	if (strings.EqualFold(project, ids.ProjectID) || strings.EqualFold(project, resolvedRepo.Project)) &&
		(strings.EqualFold(repository, ids.RepositoryID) || strings.EqualFold(repository, resolvedRepo.Name)) {
		return nil
	}
	return outside
}

// adoPullRequestURLUnverifiable is the refusal for a GUID-form URL whose ids
// could not be read: it names the reason and the web URL an operator can
// open to confirm the pull request instead.
func adoPullRequestURLUnverifiable(repo apiv1.RepoRef, rawURL string, number int, repoDisplay string, reason error) error {
	web := "https://dev.azure.com/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Project) +
		"/_git/" + url.PathEscape(repo.Name) + "/pullrequest/" + strconv.Itoa(number)
	return fmt.Errorf("pull request #%d resolved to an Azure DevOps API URL that names its project or repository by id (%s), which could not be checked against configured repository %s: %w; open the pull request at its web URL %s to confirm it belongs there",
		number, rawURL, repoDisplay, reason, web)
}
