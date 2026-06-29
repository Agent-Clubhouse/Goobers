// Package backlog is the single, documented adapter between the provider-layer
// work-item model and the runtime wire envelope. It exists to give the codebase
// exactly one conversion path from providers.WorkItem to api/v1alpha1.BacklogItem,
// replacing the ad-hoc per-caller bridges that would otherwise accrete in the
// scheduler and goober runtime (M15).
//
// # Why a separate package
//
// The two types are deliberately distinct layers, not duplicates:
//
//   - providers.WorkItem is the rich provider-ingest model: full provider
//     fidelity including hierarchy, links, status mirror, assignee, and a
//     provider-native Raw passthrough. It is coupled to the providers package.
//   - api/v1alpha1.BacklogItem is the minimal cross-component wire envelope. It
//     is pinned to a cross-language JSON Schema (api/schemas/) and is imported by
//     the engine and goober runtime *without* pulling in the providers package.
//
// Neither type's own package can host the conversion without creating an import
// that breaks that layering: putting it in api/v1alpha1 would make the wire
// envelope import providers (the very coupling the envelope exists to avoid);
// putting it in providers would make the provider layer import the API types.
// This neutral package imports both and is depended on by callers (scheduler,
// runtime), so the layering stays clean and acyclic.
package backlog

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// FromWorkItem projects a provider-layer work item onto the canonical wire
// envelope handed to a run. The projection is intentionally lossy: the wire
// envelope carries only the provider-neutral fields that components downstream
// of the scheduler need and that the cross-language JSON Schema defines.
//
// Carried through (1:1 with the BacklogItem wire contract):
//
//	ID, Provider, Title, Body, URL, Labels
//
// Intentionally dropped — these stay in the provider layer and are not part of
// the wire contract: ExternalID, Type, State, Status, Assignee, Links, Parent,
// Hierarchy, UpdatedAt, and the provider-native Raw passthrough. Downstream
// components route on Labels and display Title/Body/URL; full provider fidelity
// remains available to the provider layer via the original WorkItem.
//
// Provider is a direct string-kind conversion: providers.ProviderKind and
// apiv1.Provider share the same underlying string values ("github", "ado").
func FromWorkItem(w providers.WorkItem) apiv1.BacklogItem {
	return apiv1.BacklogItem{
		ID:       w.ID,
		Provider: apiv1.Provider(w.Provider),
		Title:    w.Title,
		Body:     w.Body,
		URL:      w.URL,
		Labels:   w.Labels,
	}
}
