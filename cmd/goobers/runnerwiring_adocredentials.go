package main

import (
	"context"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// newADOCredentialSource builds the configured Azure DevOps credential source
// for one repository. A package var so CLI tests substitute a fake per auth
// kind (an Azure CLI runner, a workload or managed identity) without a live
// Azure login; production resolves exactly as adoauth.Source does everywhere
// else.
var newADOCredentialSource = func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
	return adoauth.Source(repo, nil, stores)
}

// adoRepositoryMintsCredential reports whether repo is an Azure DevOps
// repository whose configured credential the daemon resolves and grants: every
// Microsoft Entra identity kind, and a PAT with a configured token. A PAT repo
// with no token, or an unsupported kind (both rejected by config validation),
// keeps the prior behaviour of backing no grant rather than failing every
// caller of buildCredentials.
func adoRepositoryMintsCredential(repo instance.RepoRef) bool {
	switch adoauth.AuthScheme(repo) {
	case adoauth.SchemeBearer:
		return true
	case adoauth.SchemeBasic:
		return repo.Token.Configured()
	default:
		return false
	}
}

// adoCredentialSourceIsDeferred reports whether building repo's source reads
// the host's Azure identity environment. Workload identity fails construction
// without its AZURE_* variables, and managed identity depends on the host, so
// both are built at first mint: display-path callers of buildCredentials
// (status, parked-run listing, counters) never mint and must not fail on a
// host that has no Azure identity. A PAT and the Azure CLI source are built at
// once, so a store-backed PAT with no store resolver still fails closed at
// construction.
func adoCredentialSourceIsDeferred(repo instance.RepoRef) bool {
	if repo.Auth == nil {
		return false
	}
	return repo.Auth.Kind == instance.ADOAuthWorkloadIdentity || repo.Auth.Kind == instance.ADOAuthManagedIdentity
}

// newADORepositoryTokenSource is the daemon-side minting source that backs an
// Azure DevOps repository's grants (docs/design/ado-parity-dsl-2-0.md §4.1),
// the ADO counterpart of newGitHubAppTokenSource. Each resolve asks the
// repository's configured credential source for a value — cached Entra tokens
// refresh before expiry, a PAT re-reads its env, file, keychain or store ref —
// and registers every form the value can travel in (the raw secret, the
// "Bearer" value, the base64 Basic value) with registrar before returning it,
// so a captured header is redacted as well as the bare token. The returned
// expiry is the Entra token's; a PAT states none, which the Injector and the
// credential plane treat as unbounded rather than expired.
//
// A package var, like newGitHubAppTokenSource, so CLI tests substitute it.
var newADORepositoryTokenSource = func(repo instance.RepoRef, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (credentials.ExpiringResolveFunc, error) {
	source := &lazyADOCredentialSource{build: func() (providers.ADOCredentialSource, error) {
		return newADOCredentialSource(repo, stores)
	}}
	if !adoCredentialSourceIsDeferred(repo) {
		if _, err := source.get(); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context) (string, time.Time, error) {
		current, err := source.get()
		if err != nil {
			return "", time.Time{}, err
		}
		credential, err := current.Credential(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		if registrar != nil {
			for _, form := range credential.ScrubForms() {
				registrar.Register([]byte(form))
			}
		}
		return credential.Secret, credential.ExpiresAt, nil
	}, nil
}

// lazyADOCredentialSource builds its source on first use and keeps it, so a
// cached Entra token is reused across resolves. A failed build is not kept: a
// later resolve retries it, so a transient identity-environment fault does not
// poison the source for the daemon's lifetime.
type lazyADOCredentialSource struct {
	mu     sync.Mutex
	build  func() (providers.ADOCredentialSource, error)
	source providers.ADOCredentialSource
}

func (l *lazyADOCredentialSource) get() (providers.ADOCredentialSource, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.source != nil {
		return l.source, nil
	}
	source, err := l.build()
	if err != nil {
		return nil, err
	}
	l.source = source
	return source, nil
}

// referenceReadBindings returns bindings with the token ref cleared for every
// Azure DevOps repository that authenticates as a Microsoft Entra identity.
// bindings is index-aligned with repos (buildCredentials appends one binding
// per configured repo, in order).
//
// A reference repository's read grant (MGV-10, #1285) feeds the worktree
// resolver's x-access-token Basic header (buildWorktreeGitEnv), which carries a
// PAT or a GitHub token but not an Entra bearer. Such a reference repository
// keeps authenticating through the gaggle's own ADO source, as it did before
// every ADO kind backed the repository grants, so its checkout is unchanged.
func referenceReadBindings(repos []instance.RepoRef, bindings []credentials.RepoBinding) []credentials.RepoBinding {
	out := append([]credentials.RepoBinding(nil), bindings...)
	for i := range out {
		if i < len(repos) && adoauth.AuthScheme(repos[i]) == adoauth.SchemeBearer {
			out[i].TokenRef = ""
		}
	}
	return out
}
