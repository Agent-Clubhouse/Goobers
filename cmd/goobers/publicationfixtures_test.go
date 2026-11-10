package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/providers"
)

// These workflow fixtures exercise real provider clients, including failure
// handlers. Refuse unmocked HTTP before DNS lookup, dialing, or redirects.
type publicationFixtureTransport struct {
	next    http.RoundTripper
	blocked func(string)
}

func (p publicationFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		if p.blocked != nil {
			p.blocked(host)
		}
		return nil, fmt.Errorf("publication fixture refused non-loopback HTTP host %q", host)
	}
	return p.next.RoundTrip(req)
}

func restrictPublicationFixtureHTTP(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = publicationFixtureTransport{next: previous, blocked: func(host string) {
		t.Errorf("publication fixture attempted non-loopback HTTP host %q", host)
	}}
	t.Cleanup(func() { http.DefaultTransport = previous })
}

// serveGitRemote ties provider ref reads and cleanup to the repository actually
// pushed by the workflow. A missing ref never falls back to a seeded local SHA.
func (s *fakeGitHubServer) serveGitRemote(remote string) {
	readTip := func(ctx context.Context, branch string) (string, error) {
		cmd := testgit.CommandContext(ctx, "-c", "safe.bareRepository=all", "rev-parse", "--verify", "--end-of-options", "refs/heads/"+branch+"^{commit}")
		cmd.Dir = remote
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	s.setBranchTipLookup(readTip)
	s.mu.Lock()
	s.branchDelete = func(ctx context.Context, branch string) error {
		sha, err := readTip(ctx, branch)
		if err != nil {
			return err
		}
		cmd := testgit.CommandContext(ctx, "-c", "safe.bareRepository=all", "update-ref", "-d", "refs/heads/"+branch, sha)
		cmd.Dir = remote
		return cmd.Run()
	}
	s.mu.Unlock()
}

// Workflow failure handlers and status use independent provider factories.
// Redirect each reachable boundary while preserving its token-source options.
func usePublicationWorkflowProviders(t *testing.T, server *fakeGitHubServer) {
	t.Helper()
	previousStage, previousStatus := newGitHubProvider, newStatusGitHubProvider
	previousEscalation, previousDelete := newEscalationPoster, newTerminalBranchDeleter
	previousAbort, previousClaim := newRunAbortLabelProvider, newTerminalClaimMarkerProvider
	newGitHubProvider = server.newGitHubProvider
	newStatusGitHubProvider = server.newGitHubProvider
	newEscalationPoster = func(token string) gate.Commenter { return server.newGitHubProvider(token) }
	newTerminalBranchDeleter = func(source providers.TokenSource) providers.BranchDeleter {
		return server.newGitHubProvider("", providers.WithTokenSource(source))
	}
	newRunAbortLabelProvider = func(source providers.TokenSource) workItemUpdater {
		return server.newGitHubProvider("", providers.WithTokenSource(source))
	}
	newTerminalClaimMarkerProvider = func(source providers.TokenSource, opts ...func(*providers.GitHubProvider)) workItemClaimReleaser {
		return server.newGitHubProvider("", append(opts, providers.WithTokenSource(source))...)
	}
	t.Cleanup(func() {
		newGitHubProvider, newStatusGitHubProvider = previousStage, previousStatus
		newEscalationPoster, newTerminalBranchDeleter = previousEscalation, previousDelete
		newRunAbortLabelProvider, newTerminalClaimMarkerProvider = previousAbort, previousClaim
	})
}

func TestPublicationFixtureHTTPRejectsNonLoopback(t *testing.T) {
	// A nil next transport makes an accidental delegation fail this test.
	transport := publicationFixtureTransport{}
	for _, host := range []string{"api.github.com", "localhost", "192.0.2.1"} {
		req, err := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "refused non-loopback") {
			t.Fatalf("external request was not rejected before transport: %v", err)
		}
	}
}
