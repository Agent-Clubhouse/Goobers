package providers

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

// ErrMutationUnresolved means a prior invocation may have reached the forge,
// but current provider evidence cannot safely establish its outcome. Callers
// must retain the receipt and stop; an absent marker does not authorize a POST.
var ErrMutationUnresolved = mutationreceipt.ErrUnresolved

type restReconciliationClient interface {
	restMutationRecorder
	restPager
	AuthenticatedLogin(context.Context) (string, error)
}

type restMutationClient struct {
	provider restReconciliationClient
	session  *mutationreceipt.Session
	kind     ProviderKind
	baseURL  string
}

func continuationClient(c any) *restMutationClient {
	switch p := c.(type) {
	case *GitHubProvider:
		if p.mutationSession != nil {
			return &restMutationClient{provider: p, session: p.mutationSession, kind: ProviderGitHub, baseURL: p.BaseURL}
		}
	case *GiteaProvider:
		if p.mutationSession != nil {
			return &restMutationClient{provider: p, session: p.mutationSession, kind: ProviderGitea, baseURL: p.BaseURL}
		}
	}
	return nil
}

func (c *restMutationClient) identity(ctx context.Context, repo RepositoryRef, action, target string, content any) (mutationreceipt.Identity, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return mutationreceipt.Identity{}, err
	}
	repository, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name)
	if err != nil {
		return mutationreceipt.Identity{}, err
	}
	// An authenticated principal is part of semantic identity, not just the API
	// host. Never adopt another account's copied marker.
	actor, err := c.provider.AuthenticatedLogin(mutationreceipt.FreshRead(ctx))
	if err != nil {
		return mutationreceipt.Identity{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return mutationreceipt.Identity{}, fmt.Errorf("mutation reconciliation requires authenticated provider identity")
	}
	return mutationreceipt.New(string(c.kind), repository, action, target, struct {
		Actor   string `json:"actor"`
		Content any    `json:"content"`
	}{strings.ToLower(actor), content})
}

// MutationFreshReadHeader lets HTTP decorators recognize uncached evidence reads.
const MutationFreshReadHeader = mutationreceipt.FreshReadHeader
