package instance

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// stageProviderCapabilities maps a deterministic stage's `goobers <verb>`
// command to the optional provider.Capability values its Go implementation
// reaches through providers.Dispatcher (docs/design/provider-contract-
// conformance.md §6, CONF-6 #2079). Mandatory capabilities — every method
// on RepoProvider/BacklogProvider/TriggerProvider — are deliberately
// omitted: every provider declares them by Go-interface construction, so
// they can never cause a preflight refusal and listing them here would add
// noise without changing any check's outcome. A stage/kind absent from this
// table needs only capabilities every provider already has.
var stageProviderCapabilities = map[string][]providers.Capability{
	"merge-pr": {
		providers.CapPRCompare,
		providers.CapPRLandingDetectPolicy,
		providers.CapPRMerge,
		providers.CapPRLandingEnqueue,
		providers.CapBranchDelete,
	},
	"merge-queue-poll":      {providers.CapPRLandingPoll},
	"update-behind-pr":      {providers.CapPRUpdateBranch, providers.CapPRCompare},
	"apply-verdict":         {providers.CapPRReviewSubmit},
	"gather-review-threads": {providers.CapPRReviewThreads},
	// backlog-query deliberately does NOT list CapBacklogBlockers here. Its
	// HasOpenWorkItemBlocker call (cmd/goobers/backlogquery.go,
	// filterDeclaredDependencyEligibility) only fires when a work item's
	// BlockedByCount != 0, so it is structurally unreachable for a backlog
	// provider that never sets BlockedByCount, and requiring the capability
	// unconditionally would refuse a config over a codepath it can never
	// actually exercise. For a provider that does set it but does not
	// declare backlog.blockers — ADO populates BlockedByCount from
	// Dependency-Reverse relations (providers/ado_workitems.go) without
	// implementing the checker, tracked by #2061 — the Dispatcher already
	// fails closed per item: the item is excluded with a warning instead of
	// being wrongly claimed, which is CONF-5's intended outcome and a
	// strictly narrower refusal than rejecting the whole config at
	// preflight. (An earlier version of this table required it
	// unconditionally, reasoning it would "start refusing correctly" once
	// the call became Dispatcher-gated — that was wrong: gating belongs on
	// whether BlockedByCount is ever non-zero for a provider, not on
	// running backlog-query at all.) A gaggle that genuinely needs the hard
	// guarantee can still opt in explicitly via the workflow's own
	// `requires.capabilities`.
}

// stageProviderCapabilityOverrides replaces the default derivation for one
// (provider, stage) pair, for a stage whose Go implementation deliberately
// reaches a DIFFERENT provider surface on that provider.
//
// The default table above was written against the GitHub meaning of each
// stage. `apply-verdict` is the case that proves per-provider derivation is
// necessary rather than tidy: on GitHub it submits a native review
// (pr.review.submit), but on Azure DevOps it deliberately does not — ADO has
// no native self-review to submit, so publishADOPassVerdict/
// publishADONonPassVerdict (cmd/goobers/applyverdict.go) publish the verdict on
// two provider-native surfaces instead: a "goobers"/"validation" PR status an
// ADO branch policy can gate on, plus the machine-readable verdict payload as a
// PR thread comment.
//
// Without this override the derived requirement was pr.review.submit, which ADO
// (correctly) does not declare, so config load REFUSED the shipped merge-review
// workflow on an ADO gaggle — rejecting a lane the code implements and
// ado-provider-parity.md documents. #2061/#2179.
var stageProviderCapabilityOverrides = map[providers.ProviderKind]map[string][]providers.Capability{
	providers.ProviderADO: {
		"apply-verdict": {providers.CapPRStatusPublish},
	},
}

// derivedStageCapabilities returns the provider capabilities stage needs on
// provider: the (provider, stage) override when one exists, else the
// provider-neutral default. An override REPLACES the default rather than
// adding to it — the point is that the stage reaches a different surface, not
// an additional one.
func derivedStageCapabilities(provider providers.ProviderKind, stage string) []providers.Capability {
	if byStage, ok := stageProviderCapabilityOverrides[provider]; ok {
		if override, ok := byStage[stage]; ok {
			return override
		}
	}
	return stageProviderCapabilities[stage]
}

// WorkflowRequiredProviderCapabilitiesFor returns the provider capabilities a
// single run of wf needs on provider (CONF-6, #2079):
// wf.Spec.Requires.Capabilities when explicitly declared — which replaces
// derivation entirely, letting an author narrow or widen it — else the union of
// each `goobers <verb>` deterministic stage's derived capabilities, resolved
// per provider.
//
// The provider is a required argument rather than an optional refinement: a
// stage can reach a DIFFERENT provider surface on a different provider (see
// stageProviderCapabilityOverrides), so "what does this workflow require" has
// no provider-neutral answer. An empty provider selects the neutral defaults
// for a caller that genuinely has no gaggle in hand. The result is sorted and
// de-duplicated.
func WorkflowRequiredProviderCapabilitiesFor(wf apiv1.Workflow, provider providers.ProviderKind) []providers.Capability {
	if wf.Spec.Requires != nil && len(wf.Spec.Requires.Capabilities) > 0 {
		out := make([]providers.Capability, len(wf.Spec.Requires.Capabilities))
		for i, c := range wf.Spec.Requires.Capabilities {
			out[i] = providers.Capability(c)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	seen := make(map[providers.Capability]struct{})
	for i := range wf.Spec.Tasks {
		t := &wf.Spec.Tasks[i]
		if t.Type != apiv1.TaskDeterministic || t.Run == nil || len(t.Run.Command) < 2 || t.Run.Command[0] != "goobers" {
			continue
		}
		for _, capability := range derivedStageCapabilities(provider, t.Run.Command[1]) {
			seen[capability] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]providers.Capability, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// isBacklogCapability reports whether c is a backlog.* capability — checked
// against a gaggle's Backlog.Provider rather than its Project.Provider,
// since a gaggle may pair different providers for its repo and its backlog
// (GaggleSpec.Project vs GaggleSpec.Backlog are independent RepoRef/
// BacklogRef values).
func isBacklogCapability(c providers.Capability) bool {
	return strings.HasPrefix(string(c), "backlog.")
}

// CheckProviderCapabilityRequirements fails closed at config-load when a
// workflow requires a provider capability its gaggle's connected provider
// does not declare (CONF-6, #2079) — the provider-capability counterpart of
// CheckCapabilityRequirements (RRQ-1/#735's runner-capability check).
// Unlike a missing runner capability, an unmet provider-capability
// requirement can never self-heal at runtime (a provider's declared
// capabilities, providers.CapabilitiesFor, are a static host fact that
// cannot change without a code deploy — no future runner joining fixes it),
// so this is checked once at config-load rather than every schedule tick:
// a config-time/park-time diagnostic naming the workflow, the missing
// capability, and the provider, never a mid-run stage error.
func CheckProviderCapabilityRequirements(set *ConfigSet) error {
	if set == nil {
		return nil
	}
	gagglesByName := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for _, g := range set.Gaggles {
		gagglesByName[g.Name] = g
	}
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		gaggle, ok := gagglesByName[wf.Spec.Gaggle]
		if !ok {
			// A dangling gaggle reference is reported by api/validate's
			// cross-check; nothing to compare a capability requirement
			// against here.
			continue
		}
		required := WorkflowRequiredProviderCapabilitiesFor(*wf, providers.ProviderKind(gaggle.Spec.Project.Provider))
		if len(required) == 0 {
			continue
		}
		projectCaps, projectOK := providers.CapabilitiesFor(providers.ProviderKind(gaggle.Spec.Project.Provider))
		backlogCaps, backlogOK := providers.CapabilitiesFor(providers.ProviderKind(gaggle.Spec.Backlog.Provider))
		for _, capability := range required {
			if isBacklogCapability(capability) {
				if !backlogOK || !backlogCaps.Has(capability) {
					return fmt.Errorf("workflow %q requires provider capability %q which backlog provider %q does not declare",
						wf.Name, capability, gaggle.Spec.Backlog.Provider)
				}
				continue
			}
			if !projectOK || !projectCaps.Has(capability) {
				return fmt.Errorf("workflow %q requires provider capability %q which provider %q does not declare",
					wf.Name, capability, gaggle.Spec.Project.Provider)
			}
		}
	}
	return nil
}
