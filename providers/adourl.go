package providers

import (
	"net/url"
	"regexp"
	"strings"
)

// Azure DevOps organization names are alphanumeric with hyphens — never
// dotted — which is what keeps a host-shaped first segment ("github.com/acme/
// web") out of the bare three-part slug below. These are the same character
// classes cmd/goobers/connect.go validated before ADO-N35 moved its
// hand-rolled parser here; they apply only to ParseADORepositoryURL (connect's
// operator-typed input). Azure DevOps itself allows project and repository
// names outside adoNamePart (non-ASCII letters, '&', ...), so the routing and
// matching callers use ParseADORemoteURL, which does not apply them.
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

// ParseADORepositoryURL resolves an Azure DevOps organization/project/
// repository coordinate for `goobers connect`, whose input an operator types
// directly. It accepts every host-anchored form ParseADORemoteURL does, plus
// the bare three-part slug <org>/<project>/<repo>, and requires each
// coordinate to match connect's conservative name character classes (the
// classes are what keep "github.com/acme/web" from reading as a slug).
func ParseADORepositoryURL(raw string) (org, project, repo string, ok bool) {
	value := strings.TrimSpace(raw)
	segments, anchored := adoHostAnchoredSegments(value)
	if !anchored {
		if value == "" || strings.Contains(value, "://") {
			return "", "", "", false
		}
		segments = strings.Split(strings.TrimSuffix(value, ".git"), "/")
	}
	org, project, repo, ok = adoIdentityFromSegments(segments)
	if !ok ||
		!adoOrganizationPart.MatchString(org) ||
		!adoNamePart.MatchString(project) ||
		!adoNamePart.MatchString(repo) {
		return "", "", "", false
	}
	return org, project, repo, true
}

// ParseADORemoteURL is the normaliser the remote-matching and credential-
// routing callers share (push-branch's adoRepoForOrigin, validate's
// remoteURLNamesRepository). It recognizes only forms anchored to an Azure
// DevOps host:
//
//   - https://dev.azure.com/<org>/<project>/_git/<repo>
//   - https://<org>.visualstudio.com/[DefaultCollection/]<project>/_git/<repo>
//     (the legacy pre-rename ADO host)
//   - the short form ADO emits when project and repository share a name:
//     https://dev.azure.com/<org>/_git/<repo> (and the visualstudio.com twin)
//   - git@ssh.dev.azure.com:v3/<org>/<project>/<repo> and
//     ssh://git@ssh.dev.azure.com/v3/<org>/<project>/<repo>
//   - <org>@vs-ssh.visualstudio.com:v3/<org>/<project>/<repo> and its ssh://
//     spelling (legacy ssh alias)
//
// A bare slug or local path is never ADO here, so a local mirror such as
// /srv/acme/web is left to the caller's generic handling. Any userinfo is
// ignored for identity (net/url keeps it out of Host). The host is matched
// case-insensitively, ".git" suffixes and trailing slashes are trimmed, and
// path segments are percent-decoded; a coordinate only has to be non-empty
// and free of '/', because Azure DevOps project and repository names are not
// limited to ASCII. ADO Server (self-hosted, arbitrary host) is out of scope —
// it reports false, same as GitHub, GitLab and Gitea.
func ParseADORemoteURL(raw string) (org, project, repo string, ok bool) {
	segments, anchored := adoHostAnchoredSegments(strings.TrimSpace(raw))
	if !anchored {
		return "", "", "", false
	}
	return adoIdentityFromSegments(segments)
}

// adoHostAnchoredSegments returns the identity segments (organization first)
// of a remote anchored to an Azure DevOps host; anchored is false for
// anything else, including bare slugs and local paths.
func adoHostAnchoredSegments(value string) (segments []string, anchored bool) {
	if m := adoSCPLikeSSH.FindStringSubmatch(value); m != nil {
		return adoPathSegments(m[2], false), true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return nil, false
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "ssh.dev.azure.com" || host == "vs-ssh.visualstudio.com":
		// ssh://<user>@<host>/v3/<org>/<project>/<repo>: the organization is
		// always a path segment for the SSH hosts, never the host label.
		segments = adoPathSegments(parsed.Path, false)
		if len(segments) > 0 && segments[0] == "v3" {
			segments = segments[1:]
		}
		return segments, true
	case host == "dev.azure.com":
		segments = adoPathSegments(parsed.Path, true)
		if len(segments) == 2 {
			segments = []string{segments[0], segments[1], segments[1]}
		}
		return segments, true
	case strings.HasSuffix(host, ".visualstudio.com"):
		organization := strings.TrimSuffix(host, ".visualstudio.com")
		segments = adoPathSegments(parsed.Path, true)
		if len(segments) == 1 {
			segments = []string{segments[0], segments[0]}
		}
		return append([]string{organization}, segments...), true
	}
	return nil, false
}

// adoPathSegments splits an Azure DevOps URL path into percent-decoded
// segments, trimming a ".git" suffix and dropping empty segments. With
// webForm it also drops the "_git" marker and the legacy DefaultCollection
// that only the HTTPS web URLs carry.
func adoPathSegments(path string, webForm bool) []string {
	var segments []string
	for _, segment := range strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/") {
		if decoded, err := url.PathUnescape(segment); err == nil {
			segment = decoded
		}
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if webForm && (segment == "_git" || strings.EqualFold(segment, "DefaultCollection")) {
			continue
		}
		segments = append(segments, segment)
	}
	return segments
}

// adoIdentityFromSegments requires exactly three non-empty coordinates, none
// containing '/' (a percent-decoded %2F would otherwise smuggle one in).
func adoIdentityFromSegments(segments []string) (org, project, repo string, ok bool) {
	trimmed := make([]string, 0, len(segments))
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if strings.Contains(segment, "/") {
			return "", "", "", false
		}
		trimmed = append(trimmed, segment)
	}
	if len(trimmed) != 3 {
		return "", "", "", false
	}
	return trimmed[0], trimmed[1], trimmed[2], true
}
