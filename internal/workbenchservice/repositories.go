package workbenchservice

import (
	"context"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// Repository builds a conditional-cache client for the exact declared repository
// from callback-scoped interactive credentials. It cannot borrow automation auth.
func (f ProviderFactory) Repository(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential) (workbenchprovider.RepositoryClient, error) {
	if binding.Source.Spec.Repository == nil {
		return nil, workbenchprovider.ErrInvalidSource
	}
	return f.provider(ctx, binding, *binding.Source.Spec.Repository, credential)
}
