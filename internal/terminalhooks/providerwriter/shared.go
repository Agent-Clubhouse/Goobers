package providerwriter

import (
	"context"

	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

// SharedClaims is the shared-claim extension of the control provider writer.
// The run's pinned repository is checked before materializing credentials.
// Returned adapters scrub provider errors and retain CAS error identities.
// The store writes only versioned coordination records; visibility writes only
// the non-authoritative claimed label.
type SharedClaims interface {
	SharedClaimStore(context.Context, string, providers.RepositoryRef) (sharedclaim.Store, error)
	SharedClaimVisibility(context.Context, string, providers.RepositoryRef) (sharedclaim.Visibility, error)
}

// SharedVisibilityReconciler discovers remote records independently of retained
// run history. It is used only by the lease-owned repository registry sweep.
type SharedVisibilityReconciler interface {
	ReconcileSharedVisibility(context.Context, providers.RepositoryRef, string, int) (string, error)
}
