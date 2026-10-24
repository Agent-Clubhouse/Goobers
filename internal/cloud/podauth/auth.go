package podauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/httpapi"
	shared "github.com/goobers/goobers/internal/podauth"
)

// GrantAdmission checks durable revocation and expiry against the same primary
// database clock used by revocation cleanup. Local wall-clock validation alone
// cannot safely admit a grant after its revocation has been pruned.
type GrantAdmission interface {
	planes.GrantRevocations
	CheckGrant(context.Context, string, time.Time) error
}

// Config configures cloud auth. A key file and durable revocations are mandatory;
// empty/registry modes fail at construction rather than admitting local state.
type Config struct {
	Mode        string
	KeyFile     string
	Revocations GrantAdmission
	Fallback    httpapi.Authenticator
}

// Auth is the cloud role's auth assembly. RunKeyReload must run for the role's
// lifetime. Signers retain their existing typed mint interfaces for dispatchers.
type Auth struct {
	PodKeys     *shared.SignedKey
	GrantKeys   *shared.SignedKey
	verifier    *shared.Authenticator
	revocations GrantAdmission
}

// New refuses process-local registry auth and loads both domain-separated keys.
// The cloud role must call it before serving any plane requests.
func New(config Config) (*Auth, error) {
	if config.Mode != "signed" || config.KeyFile == "" {
		return nil, errors.New("cloud requires signed pod tokens with a shared key file; Registry mode is unsupported")
	}
	if config.Revocations == nil {
		return nil, errors.New("cloud requires durable grant revocation")
	}
	keys, err := shared.LoadKeyRingFile(config.KeyFile, "")
	if err != nil {
		return nil, err
	}
	grants, err := shared.LoadKeyRingFile(config.KeyFile, shared.CredentialGrantKeyDomain)
	if err != nil {
		return nil, err
	}
	verifier, err := shared.NewAuthenticator(keys, config.Fallback)
	if err != nil {
		return nil, err
	}
	// Child-workflow grants share the credential-grant key domain; their
	// revocation is the bound custody row, checked by the child-workflow
	// service on every request, not the grant revocation table.
	verifier = verifier.WithCredentialGrants(grants).WithChildWorkflowGrants(grants)
	return &Auth{PodKeys: keys, GrantKeys: grants, verifier: verifier, revocations: config.Revocations}, nil
}

// Authenticate verifies grants and their durable revocation before minting a
// principal. All other bearer forms retain shared podauth routing and scope rules.
func (a *Auth) Authenticate(request *http.Request) (*httpapi.Principal, error) {
	if a == nil || a.verifier == nil {
		return nil, errors.New("cloud pod auth is not initialized")
	}
	token := ""
	authorization := request.Header.Get("Authorization")
	if len(authorization) > 7 && strings.EqualFold(authorization[:7], "Bearer ") {
		token = strings.TrimSpace(authorization[7:])
	}
	if shared.IsCredentialGrant(token) {
		grant, err := a.VerifyCredentialGrant(request.Context(), token)
		if err != nil {
			return nil, err
		}
		return &httpapi.Principal{Subject: "run:" + grant.RunID, Issuer: httpapi.CredentialGrantPrincipalIssuer}, nil
	}
	return a.verifier.Authenticate(request)
}

// VerifyCredentialGrant is also the admission seam for the cloud credential
// refresh service: signature, expiry, and revocation all precede any resolution.
func (a *Auth) VerifyCredentialGrant(ctx context.Context, token string) (shared.CredentialGrant, error) {
	grant, err := a.GrantKeys.VerifyCredentialGrant(token)
	if err != nil {
		return shared.CredentialGrant{}, err
	}
	if err := a.revocations.CheckGrant(ctx, grant.ID, grant.ExpiresAt); err != nil {
		return shared.CredentialGrant{}, err
	}
	return grant, nil
}

// RunKeyReload watches both MAC domains until cancellation. Failure reports must
// be visible in role health/telemetry; malformed replacements keep last good keys.
func (a *Auth) RunKeyReload(ctx context.Context, report func(error)) error {
	if report == nil {
		return errors.New("cloud key reload requires an error reporter")
	}
	var wg sync.WaitGroup
	for _, key := range []*shared.SignedKey{a.PodKeys, a.GrantKeys} {
		wg.Go(func() { _ = key.WatchKeyFile(ctx, shared.KeyReloadInterval, report) })
	}
	wg.Wait()
	return ctx.Err()
}
