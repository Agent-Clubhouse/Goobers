package eventing

import (
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func configuredPolicy() *apiv1.GaggleEvents {
	return &apiv1.GaggleEvents{Subscriptions: []apiv1.EventSubscription{{Name: "review-pr", Workflow: "review", Filter: apiv1.EventSubscriptionFilter{
		All: []apiv1.EventAttributeMatch{{Attribute: "source", Equals: "urn:excluded", Not: true}},
		Any: []apiv1.EventAttributeMatch{{Attribute: "type", Equals: "pr.updated"}, {Attribute: "type", Equals: "pr.ready"}},
	}, Debounce: &apiv1.EventDebounce{KeyAttribute: "subject"}}}}
}

func TestConfiguredCatalogPinsTargetsAndAppliesDefaults(t *testing.T) {
	policy := configuredPolicy()
	resolve := func(name string) (TargetPins, error) {
		if name != "review" {
			t.Fatal("unexpected workflow", name)
		}
		return TargetPins{"workflow-pin", "goober-pin"}, nil
	}
	catalog, err := CompileConfiguredCatalog("team", "generation-one", policy, resolve)
	if err != nil {
		t.Fatal(err)
	}
	policy.Subscriptions[0].Filter.Any[0].Equals = "changed"
	policy.Subscriptions[0].Debounce.KeyAttribute = "id"
	raw := []byte(`{"specversion":"1.0","id":"one","source":"urn:repo","type":"pr.updated","subject":"pr/1"}`)
	_, plan, err := catalog.Match("team", raw)
	if err != nil || len(plan.Routes) != 1 {
		t.Fatal(plan, err)
	}
	route := plan.Routes[0]
	if route.ConfigGeneration != "generation-one" || route.WorkflowDigest != "workflow-pin" || route.GooberDigest != "goober-pin" || !strings.HasPrefix(route.Revision, "sha256:") {
		t.Fatal("pins were not retained", route)
	}
	if route.Debounce.Window != 5*time.Second || route.Debounce.MaxWait != 30*time.Second || route.Debounce.MaxEvents != 100 || route.Debounce.InputMode != "all" {
		t.Fatal("defaults differ", route.Debounce)
	}
	_, unmatched, err := catalog.Match("team", []byte(strings.Replace(string(raw), "urn:repo", "urn:excluded", 1)))
	if err != nil || len(unmatched.Routes) != 0 {
		t.Fatal("negated all condition was ignored", unmatched, err)
	}
	newCatalog, err := CompileConfiguredCatalog("team", "generation-two", configuredPolicy(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	_, next, _ := newCatalog.Match("team", raw)
	if next.Routes[0].Revision == route.Revision {
		t.Fatal("changed retained generation reused old consumer revision")
	}
}

func TestConfiguredCatalogRejectsInvalidDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*apiv1.GaggleEvents)
	}{
		{"duplicate", func(p *apiv1.GaggleEvents) { p.Subscriptions = append(p.Subscriptions, p.Subscriptions[0]) }},
		{"path name", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Name = "../other" }},
		{"empty filter", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Filter = apiv1.EventSubscriptionFilter{} }},
		{"data selector", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Filter.All[0].Attribute = "data.gaggle" }},
		{"empty equality", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Filter.Any[0].Equals = "" }},
		{"too many conditions", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Filter.All = make([]apiv1.EventAttributeMatch, 16) }},
		{"bad duration", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.Window = "later" }},
		{"zero window", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.Window = "0s" }},
		{"inverted deadline", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.Window = "1m" }},
		{"no key", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.KeyAttribute = "" }},
		{"two keys", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.ConstantKey = "all" }},
		{"zero events", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.MaxEvents = new(int32) }},
		{"bad mode", func(p *apiv1.GaggleEvents) { p.Subscriptions[0].Debounce.InputMode = "first" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := configuredPolicy()
			tc.edit(policy)
			if err := ValidateConfiguration(policy); err == nil {
				t.Fatal("invalid subscription accepted")
			}
		})
	}
	if _, err := CompileConfiguredCatalog("team", "generation", configuredPolicy(), func(string) (TargetPins, error) {
		return TargetPins{}, errors.New("target missing from gaggle")
	}); err == nil {
		t.Fatal("unresolved target accepted")
	}
	if err := ValidateConfiguration(nil); err != nil {
		t.Fatal("omitted policy changed compatibility", err)
	}
}

func TestConfiguredFilterSupportsMaximumNegatedConditions(t *testing.T) {
	p := configuredPolicy()
	p.Subscriptions[0].Filter.All = nil
	p.Subscriptions[0].Filter.Any = nil
	for range 15 {
		match := apiv1.EventAttributeMatch{Attribute: "type", Equals: "excluded", Not: true}
		p.Subscriptions[0].Filter.All = append(p.Subscriptions[0].Filter.All, match)
		p.Subscriptions[0].Filter.Any = append(p.Subscriptions[0].Filter.Any, match)
	}
	if err := ValidateConfiguration(p); err != nil {
		t.Fatal("documented maximum does not fit matcher bounds", err)
	}
}
