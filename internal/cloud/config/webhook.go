package config

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// WebhookSubscription is one eligible workflow's GitHub webhook trigger,
// resolved against the repository its gaggle targets.
type WebhookSubscription struct {
	Gaggle, Workflow string
	Owner, Name      string
	// Events are the GitHub event names the trigger subscribes to.
	Events []string
}

// matches reports whether the subscription takes event from owner/name.
func (w WebhookSubscription) matches(event, owner, name string) bool {
	if !strings.EqualFold(w.Owner, owner) || !strings.EqualFold(w.Name, name) {
		return false
	}
	for _, e := range w.Events {
		if e == event {
			return true
		}
	}
	return false
}

// webhookSubscriptions derives the subscriptions of the generation's eligible
// workflows. A disabled trigger, or a gaggle that does not target github.com,
// subscribes to nothing.
func webhookSubscriptions(set *instance.ConfigSet, eligible map[string]struct{}) []WebhookSubscription {
	projects := make(map[string]apiv1.RepoRef, len(set.Gaggles))
	for i := range set.Gaggles {
		projects[set.Gaggles[i].Name] = set.Gaggles[i].Spec.Project
	}
	var out []WebhookSubscription
	for i := range set.Workflows {
		w := &set.Workflows[i]
		if _, ok := eligible[workflowKey(w.Spec.Gaggle, w.Name)]; !ok {
			continue
		}
		project, ok := projects[w.Spec.Gaggle]
		if !ok || project.Provider != apiv1.ProviderGitHub {
			continue
		}
		for _, trigger := range w.Spec.Triggers {
			if trigger.Type != apiv1.TriggerWebhook || (trigger.Enabled != nil && !*trigger.Enabled) || len(trigger.Events) == 0 {
				continue
			}
			out = append(out, WebhookSubscription{
				Gaggle: w.Spec.Gaggle, Workflow: w.Name, Owner: project.Owner, Name: project.Name,
				Events: append([]string(nil), trigger.Events...),
			})
		}
	}
	return out
}

// ErrNoGeneration means no configuration generation has been applied yet.
var ErrNoGeneration = errors.New("cloud configuration: no generation is applied")

// WebhookRecipients returns the applied generation's digest and the eligible
// workflows subscribed to event on owner/name. It fails closed before the
// first apply, so a delivery is never answered from an unassessed generation.
func (c *Configuration) WebhookRecipients(event, owner, name string) (string, []WebhookSubscription, error) {
	gen := c.applier.Guard().current.Load()
	if gen == nil {
		return "", nil, fmt.Errorf("%w: %w", ErrIneligible, ErrNoGeneration)
	}
	var out []WebhookSubscription
	for _, s := range gen.webhooks {
		if s.matches(event, owner, name) {
			out = append(out, s)
		}
	}
	return gen.digest, out, nil
}
