package providers

import (
	"net/url"
	"regexp"
	"strings"
)

// Azure DevOps organization names are alphanumeric with hyphens — never
// dotted — which is what keeps a host-shaped first segment ("github.com/acme/
// web") out of the three-part branch below. Project and repository names are
// looser (dots, underscores and spaces are legal). These are the same
// character classes cmd/goobers/connect.go validated before ADO-N35 moved
// every hand-rolled ADO URL parser onto ParseADORepositoryURL.
var (
	adoOrganizationPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	adoNamePart         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_. -]*$`)
)

// adoSCPLikeSSH matches the scp-like Azure DevOps SSH remote forms:
// git@ssh.dev.azure.com:v3/<org>/<project>/<repo> and the legacy
// <org>@vs-ssh.visualstudio.com:v3/<org>/<project>/<repo>. The username
// before '@' is never part of the identity — ADO always uses the literal
// "git" for ssh.dev.azure.com and repeats the organization for the legacy
// vs-ssh.visualstudio.com alias — so it is discarded; only the host and the
// "v3/..." coordinate after it matter.
var adoSCPLikeSSH = regexp.MustCompile(`(?i)^[^@/]+@(ssh\.dev\.azure\.com|vs-ssh\.visualstudio\.com):v3/(.+)$`)

// ParseADORepositoryURL is the one normaliser every Azure DevOps remote/URL
// parser in this binary is built on (cmd/goobers/connect.go's
// connectADOIdentity, pushbranch.go's adoRepoForOrigin, validate.go's
// remoteURLNamesRepository). It resolves the organization/project/repository
// coordinate out of:
//
//   - https://dev.azure.com/<org>/<project>/_git/<repo>
//   - https://<org>.visualstudio.com/[DefaultCollection/]<project>/_git/<repo>
//     (the legacy pre-rename ADO host)
//   - git@ssh.dev.azure.com:v3/<org>/<project>/<repo>
//   - <org>@vs-ssh.visualstudio.com:v3/<org>/<project>/<repo> (legacy ssh alias)
//   - the short dev.azure.com form ADO emits when project and repository
//     share a name: https://dev.azure.com/<org>/_git/<repo>
//   - the bare three-part slug an operator types directly: <org>/<project>/<repo>
//
// Any URL form's userinfo (e.g. https://<org>@dev.azure.com/...) is accepted
// and ignored: the organization is read from the path or host as usual, not
// from the userinfo, and net/url already keeps userinfo out of Host. Matching
// is case-insensitive on the host, ".git" suffixes and trailing slashes are
// trimmed, and path segments are percent-decoded. ADO Server (self-hosted,
// arbitrary host) is out of scope — it reports false, same as GitHub, GitLab,
// Gitea and typo'd inputs.
func ParseADORepositoryURL(raw string) (org, project, repo string, ok bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", "", false
	}
	if m := adoSCPLikeSSH.FindStringSubmatch(value); m != nil {
		return adoIdentityFromSegments(strings.Split(strings.TrimSuffix(m[2], ".git"), "/"))
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		host := strings.ToLower(parsed.Hostname())
		segments := adoPathSegments(parsed.Path)
		switch {
		case host == "dev.azure.com" || host == "ssh.dev.azure.com":
			// https://dev.azure.com/<org>/<project>/_git/<repo>, and the short
			// form ADO itself emits when project and repository share a name:
			// https://dev.azure.com/<org>/_git/<repo>.
			if len(segments) == 2 {
				segments = []string{segments[0], segments[1], segments[1]}
			}
			return adoIdentityFromSegments(segments)
		case strings.HasSuffix(host, ".visualstudio.com"):
			organization := strings.TrimSuffix(host, ".visualstudio.com")
			if len(segments) == 1 {
				segments = []string{segments[0], segments[0]}
			}
			return adoIdentityFromSegments(append([]string{organization}, segments...))
		}
		return "", "", "", false
	}
	return adoIdentityFromSegments(strings.Split(strings.TrimSuffix(value, ".git"), "/"))
}

// adoPathSegments splits an Azure DevOps URL path into identity segments,
// dropping the "_git" marker and the legacy DefaultCollection.
func adoPathSegments(path string) []string {
	var segments []string
	for _, segment := range strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/") {
		decoded, err := url.PathUnescape(segment)
		if err == nil {
			segment = decoded
		}
		if segment == "" || segment == "_git" || strings.EqualFold(segment, "DefaultCollection") {
			continue
		}
		segments = append(segments, segment)
	}
	return segments
}

func adoIdentityFromSegments(segments []string) (org, project, repo string, ok bool) {
	trimmed := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment = strings.TrimSpace(segment); segment != "" {
			trimmed = append(trimmed, segment)
		}
	}
	if len(trimmed) != 3 ||
		!adoOrganizationPart.MatchString(trimmed[0]) ||
		!adoNamePart.MatchString(trimmed[1]) ||
		!adoNamePart.MatchString(trimmed[2]) {
		return "", "", "", false
	}
	return trimmed[0], trimmed[1], trimmed[2], true
}
