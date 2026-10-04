package validate

import "github.com/goobers/goobers/internal/eventing"

const errorEventSubscriptions WarningCode = "EVT001"

func (ix *index) checkEventSubscriptions(r *Report) {
	for _, name := range sortedGaggleNames(ix.gaggles) {
		policy := ix.gaggles[name].Spec.Events
		if err := eventing.ValidateConfiguration(policy); err != nil {
			r.add(errorEventSubscriptions, Error, ix.gaggleFile[name], "Gaggle", name, "spec.events: %v", err)
			continue
		}
		if policy == nil {
			continue
		}
		for _, subscription := range policy.Subscriptions {
			if _, found := ix.workflows[workflowIdentity{gaggle: name, name: subscription.Workflow}]; !found {
				ix.referenceNotFound(r, errorEventSubscriptions, ix.gaggleFile[name], "Gaggle", name,
					"spec.events subscription %q names workflow %q, but no workflow with that name exists in this gaggle", subscription.Name, subscription.Workflow)
			}
		}
	}
}
