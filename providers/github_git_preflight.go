package providers

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// preflightGitReceivePack checks service access, NOT a branch update. GitHub
// requires Contents:write for installation tokens to discover receive-pack;
// repo user-role permissions do not describe that grant. This GET never sends
// a pack, creates a ref, or invokes receive-pack's mutating POST. Branch rules
// are still checked by the caller, and the eventual push remains authoritative.
func (p *GitHubProvider) preflightGitReceivePack(ctx context.Context, repo RepositoryRef) (RepositoryWritePreflightResult, error) {
	endpoint, err := githubReceivePackEndpoint(p.BaseURL, repo)
	if err != nil {
		return gitPreflightUnavailable("cannot determine a trusted Git discovery endpoint"), nil
	}
	token, err := p.resolveToken(ctx)
	if err != nil {
		return RepositoryWritePreflightResult{}, err
	}
	if token == "" {
		return RepositoryWritePreflightResult{FailureCapability: RepoWriteFailureUnauthorized, Detail: "Git discovery requires a repository credential"}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RepositoryWritePreflightResult{}, err
	}
	req.SetBasicAuth("x-access-token", token)
	req.Header.Set("Accept", "application/x-git-receive-pack-advertisement")
	req.Header.Set("Cache-Control", "no-cache")

	// Git smart HTTP is not the REST API: do not use REST response caches or
	// API quota middleware. Reuse an explicitly configured transport (e.g. an
	// enterprise CA), but never follow redirects or inherit cookies. In
	// particular, Go's default redirect policy forwards auth to subdomains.
	client := newProviderHTTPClient(defaultProviderHTTPTimeout)
	if configured, ok := p.Client.(*http.Client); ok {
		client.Transport = configured.Transport
		if configured.Timeout > 0 && configured.Timeout < client.Timeout {
			client.Timeout = configured.Timeout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// Do not echo a remote body, URL, or transport error containing auth.
		if ctx.Err() != nil {
			return RepositoryWritePreflightResult{}, ctx.Err()
		}
		return gitPreflightUnavailable("Git push-service discovery could not be completed"), nil
	}
	defer func() { _ = resp.Body.Close() }()
	if isRateLimited(resp) {
		return gitPreflightUnavailable("Git push-service discovery is rate limited"), nil
	}
	if ctx.Err() != nil {
		return RepositoryWritePreflightResult{}, ctx.Err()
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusNotFound:
		return RepositoryWritePreflightResult{FailureCapability: RepoWriteFailureUnauthorized, Detail: fmt.Sprintf("Git repository unreachable or credential unauthorized (HTTP %d)", resp.StatusCode)}, nil
	case http.StatusForbidden:
		return RepositoryWritePreflightResult{FailureCapability: RepoWriteFailureNoPushPermission, Detail: "credential cannot access this repository's Git push service (HTTP 403)"}, nil
	case http.StatusOK:
		// A 200 HTML login page, dumb-Git response, or upload-pack advert
		// proves nothing. Inspect only the fixed-size service announcement:
		// advertised refs are not needed and can grow without a useful bound.
		const announcement = "001f# service=git-receive-pack\n0000"
		mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err == nil && mediaType == "application/x-git-receive-pack-advertisement" {
			var prefix [len(announcement)]byte
			if _, err := io.ReadFull(resp.Body, prefix[:]); err == nil && string(prefix[:]) == announcement {
				return RepositoryWritePreflightResult{OK: true}, nil
			}
		}
	}
	if ctx.Err() != nil {
		return RepositoryWritePreflightResult{}, ctx.Err()
	}
	return gitPreflightUnavailable(fmt.Sprintf("Git push-service discovery did not return a valid advertisement (HTTP %d)", resp.StatusCode)), nil
}

func gitPreflightUnavailable(detail string) RepositoryWritePreflightResult {
	return RepositoryWritePreflightResult{FailureCapability: RepoWriteFailurePolicyIntrospectionUnavailable, Detail: detail}
}

// Derive the endpoint from configured provider authority, never repo.URL or
// a server-supplied clone_url. Neither may redirect a credential to another
// host/repository. GitHub.com, data-residency hosts, and GHES /api/v3 map to
// their HTTPS Git origins. Plain HTTP is only allowed for loopback fixtures.
func githubReceivePackEndpoint(base string, repo RepositoryRef) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid GitHub API origin")
	}
	loopback := net.ParseIP(u.Hostname()).IsLoopback() || u.Hostname() == "localhost"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return "", fmt.Errorf("git discovery requires HTTPS")
	}
	for _, segment := range []string{repo.Owner, repo.Name} {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\?#%") {
			return "", fmt.Errorf("invalid repository path segment")
		}
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case u.Host == "api.github.com" && path == "":
		u.Host = "github.com"
	case strings.HasPrefix(u.Host, "api.") && strings.HasSuffix(u.Host, ".ghe.com") && path == "":
		u.Host = strings.TrimPrefix(u.Host, "api.")
	case strings.HasSuffix(path, "/api/v3"):
		path = strings.TrimSuffix(path, "/api/v3")
	case loopback && path == "":
	default:
		return "", fmt.Errorf("unrecognized GitHub API origin")
	}
	u.Path = path + "/" + repo.Owner + "/" + repo.Name + ".git/info/refs"
	u.RawPath = ""
	u.RawQuery = "service=git-receive-pack"
	return u.String(), nil
}
