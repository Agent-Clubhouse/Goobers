package workbenchservice

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/apireadcache"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

// ProviderFactory reuses the shared conditional read cache. Interactive reads
// always revalidate against the provider; they never reuse an hour-long scheduler
// evaluation snapshot. Authorization precedes this factory on every call.
type ProviderFactory struct {
	SchedulerDirectory string
	Client             providers.HTTPClient
	Registrar          providers.SecretRegistrar
}

// Backlog builds an exact-target provider from the supplied interactive credential.
func (f ProviderFactory) Backlog(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential) (workbenchprovider.BacklogClient, error) {
	return f.provider(ctx, binding, binding.Source.BacklogIdentity, credential)
}

type sourceProvider interface {
	workbenchprovider.BacklogClient
	workbenchprovider.RepositoryClient
}

func (f ProviderFactory) provider(ctx context.Context, binding ReadBinding, target apiv1.InteractiveRepositoryIdentity, credential interactiveaccess.Credential) (sourceProvider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credential.Value == "" || (!credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(time.Now())) {
		return nil, interactiveaccess.ErrCredentialUnavailable
	}
	scope := apireadcache.Scope{Gaggle: binding.Scope.GaggleID, Binding: "interactive:" + binding.Source.Spec.Name, Generation: binding.Generation}
	switch target.Provider {
	case "github":
		if credential.Scheme != "bearer" {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		p := providers.NewGitHubProvider(credential.Value)
		if f.Client != nil {
			p.Client = f.Client
		}
		p.Client = apireadcache.ScopedClient(f.SchedulerDirectory, "", scope, providers.ProviderGitHub, p.Client)
		return p, nil
	case "ado":
		kind := providers.ADOCredentialKindBearer
		if credential.Scheme == "basic" {
			kind = providers.ADOCredentialKindPAT
		} else if credential.Scheme != "bearer" {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		source, err := providers.NewADODeliveredCredentialSourceWithExpiry(kind, credential.Value, "interactive workbench read", credential.ExpiresAt)
		if err != nil {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		p := providers.NewADOProvider(target.Owner, target.Project, "", providers.WithADOCredentialSource(source), providers.WithADOSecretRegistrar(f.Registrar))
		if f.Client != nil {
			p.Client = f.Client
		}
		p.Client = apireadcache.ScopedClient(f.SchedulerDirectory, "", scope, providers.ProviderADO, p.Client)
		return p, nil
	default:
		return nil, interactiveaccess.ErrCredentialUnavailable
	}
}
