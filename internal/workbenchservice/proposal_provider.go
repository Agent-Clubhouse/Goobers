package workbenchservice

import (
	"context"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// RepositoryProposal builds the same scoped conditional-cache provider used for
// source browsing. The adapter explicitly disables automatic mutation retries.
func (f ProviderFactory) RepositoryProposal(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential) (workbenchprovider.RepositoryProposalClient, error) {
	client, err := f.Repository(ctx, binding, credential)
	if err != nil {
		return nil, err
	}
	proposal, ok := client.(workbenchprovider.RepositoryProposalClient)
	if !ok {
		return nil, workbenchprovider.ErrUnsupportedEdit
	}
	return proposal, nil
}
