package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/providers"
)

type landingVerificationProvider struct {
	*providers.GitHubProvider
	merged     bool
	unreadable bool
	reads      int
}

func (p *landingVerificationProvider) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.reads++
	if p.unreadable {
		return providers.PullRequestPollResult{}, errors.New("read unavailable")
	}
	return providers.PullRequestPollResult{Merged: p.merged, HeadSHA: "head"}, nil
}

func TestLandingErrorPublicationCustody(t *testing.T) {
	for _, mode := range []string{"file", "plane"} {
		for _, tc := range []struct {
			name                         string
			status                       int
			merged, unreadable, retained bool
		}{
			{name: "transport after accept", retained: true},
			{name: "transport and unreadable state", unreadable: true, retained: true},
			{name: "transport then confirmed merged", merged: true},
			{name: "explicit 405 refusal", status: 405},
			{name: "explicit 409 refusal", status: 409},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				plane := newClaimsPlane(t)
				local, err := fileClaimLedger(plane.layout)
				if err != nil {
					t.Fatal(err)
				}
				ledger := local
				token := plane.admitRun(t, "test", "first")
				if mode == "plane" {
					ledger = plane.client(t, "first", token)
				}
				lock := claimsclient.MergeLock{Key: claimsclient.MergeLockKey("test", "github", "publication/github/acme", "repo"), RunID: "first", Workflow: "merge-review"}
				var accepted atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						_, _ = fmt.Fprint(w, `{"message":"Pull Request is not mergeable"}`)
						return
					}
					accepted.Store(true) // Provider accepted the mutation, then lost the reply.
					conn, _, hijackErr := w.(http.Hijacker).Hijack()
					if hijackErr != nil {
						t.Error(hijackErr)
						return
					}
					if closeErr := conn.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				}))
				defer server.Close()
				github := providers.NewGitHubProvider("token", providers.WithMaxTransientRetries(0), func(p *providers.GitHubProvider) { p.BaseURL = server.URL; p.Client = server.Client() })
				provider := &landingVerificationProvider{GitHubProvider: github, merged: tc.merged, unreadable: tc.unreadable}
				lander, err := mergepolicy.ForPolicy(providers.MergePolicyDirect)
				if err != nil {
					t.Fatal(err)
				}
				var landErr error
				custodyErr := withPublicationClaim(t.Context(), ledger, lock, time.Second, 5*time.Second, 10*time.Second, func(ctx context.Context) error {
					_, landErr = landWithPublicationCustody(ctx, lander, providers.NewDispatcher(provider), mergepolicy.Request{Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}, PullID: "9", ExpectedHeadSHA: "head"})
					return nil // The command reports this original error through its usual path.
				})
				if custodyErr != nil || landErr == nil {
					t.Fatalf("custody=%v landing=%v", custodyErr, landErr)
				}
				if tc.status == 0 && (!accepted.Load() || provider.reads != 1) {
					t.Fatalf("accepted=%v reads=%d", accepted.Load(), provider.reads)
				}
				if tc.status != 0 && provider.reads != 0 {
					t.Fatal("explicit refusal performed ambiguous-state read")
				}
				entries, err := ledger.ForRunAll(t.Context(), "first")
				if err != nil {
					t.Fatal(err)
				}
				if (len(entries) != 0) != tc.retained {
					t.Fatalf("retained=%v entries=%+v", tc.retained, entries)
				}
			})
		}
	}
}
