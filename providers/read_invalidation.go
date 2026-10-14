package providers

import (
	"context"
	"strings"
)

// ProviderMutationObserver receives confirmed provider effects independently of
// journal recording. Implementations may invalidate disposable reads, but must
// not reinterpret an external success as a refusal or grant write authority.
type ProviderMutationObserver interface {
	// ObserveProviderMutation reports a confirmed effect without changing its result.
	ObserveProviderMutation(context.Context, ProviderKind)
}

func observeProviderMutation(ctx context.Context, client HTTPClient, kind ProviderKind) {
	if observer, ok := client.(ProviderMutationObserver); ok {
		observer.ObserveProviderMutation(ctx, kind)
	}
}

func adoAutoCompleteAcknowledged(out adoPullRequestDetail, identityID string) bool {
	return strings.EqualFold(out.Status, "completed") ||
		out.AutoCompleteSetBy != nil && identityID != "" && out.AutoCompleteSetBy.ID == identityID
}
