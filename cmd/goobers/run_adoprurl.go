package main

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// adoGUIDSegment matches the GUID Azure DevOps uses to name a project or
// repository in its API URLs.
var adoGUIDSegment = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

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
	project, repository, ok := adoAPIPullRequestURLByID(resolvedRepo, rawURL, number)
	if !ok {
		return outside
	}
	unverifiable := func(reason error) error {
		return fmt.Errorf("pull request #%d resolved to an Azure DevOps API URL that names its project or repository by id (%s), which could not be checked against configured repository %s: %w; open the pull request at its web URL %s to confirm it belongs there",
			number, rawURL, repoDisplay, reason, adoPullRequestWebURL(resolvedRepo, number))
	}
	reader, ok := provider.(adoRepositoryIDReader)
	if !ok {
		return unverifiable(fmt.Errorf("provider %q cannot read repository ids", repo.Provider))
	}
	ids, err := reader.RepositoryIDs(ctx, repo)
	if err != nil {
		return unverifiable(err)
	}
	projectMatches := strings.EqualFold(project, ids.ProjectID) || strings.EqualFold(project, resolvedRepo.Project)
	repositoryMatches := strings.EqualFold(repository, ids.RepositoryID) || strings.EqualFold(repository, resolvedRepo.Name)
	if projectMatches && repositoryMatches {
		return nil
	}
	return outside
}

// adoAPIPullRequestURLByID extracts the project and repository segments of
// an Azure DevOps API-form pull request URL
// (<org>/<project>/_apis/git/repositories/<repository>/pullRequests/<n>) in
// the configured organization for pull request number, when at least one of
// them is a GUID — the shape adoAPIPullRequestURLMatchesRepository cannot
// verify by name.
func adoAPIPullRequestURLByID(repo apiv1.RepoRef, rawURL string, number int) (project, repository string, ok bool) {
	if repo.Provider != apiv1.ProviderADO {
		return "", "", false
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return "", "", false
	}
	host := strings.ToLower(parsed.Hostname())
	visualStudioHost := strings.ToLower(repo.Owner) + ".visualstudio.com"
	if host != "dev.azure.com" && host != visualStudioHost {
		return "", "", false
	}
	parts := strings.FieldsFunc(parsed.EscapedPath(), func(r rune) bool { return r == '/' })
	for i := range parts {
		if parts[i], err = url.PathUnescape(parts[i]); err != nil {
			return "", "", false
		}
	}
	numberText := strconv.Itoa(number)
	for i := 1; i+5 < len(parts); i++ {
		organizationMatches := host == visualStudioHost || (i >= 2 && strings.EqualFold(parts[i-2], repo.Owner))
		if organizationMatches &&
			strings.EqualFold(parts[i], "_apis") &&
			strings.EqualFold(parts[i+1], "git") &&
			strings.EqualFold(parts[i+2], "repositories") &&
			strings.EqualFold(parts[i+4], "pullRequests") &&
			parts[i+5] == numberText &&
			(adoGUIDSegment.MatchString(parts[i-1]) || adoGUIDSegment.MatchString(parts[i+3])) {
			return parts[i-1], parts[i+3], true
		}
	}
	return "", "", false
}

// adoPullRequestWebURL is the browser URL of pull request number in the
// configured repository, the form an operator can open to confirm it.
func adoPullRequestWebURL(repo apiv1.RepoRef, number int) string {
	return "https://dev.azure.com/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Project) +
		"/_git/" + url.PathEscape(repo.Name) + "/pullrequest/" + strconv.Itoa(number)
}
