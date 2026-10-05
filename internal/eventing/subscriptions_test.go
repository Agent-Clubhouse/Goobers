package eventing

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func subscriptionFixture(name string) Subscription {
	return Subscription{Target: Route{Consumer: name, Revision: "consumer-v1", Workflow: "review", WorkflowDigest: "workflow-pin", GooberDigest: "goober-pin", ConfigGeneration: "generation-pin"}, Filter: Filter{Attribute: "type", Equals: "pr.updated"}}
}
func debounceFixture(attribute string) *DebouncePolicy {
	return &DebouncePolicy{KeyAttribute: attribute, Window: time.Second, MaxWait: time.Minute, MaxEvents: 10, InputMode: "latest"}
}

func TestCatalogPinsIndependentConsumersAndMissingKeys(t *testing.T) {
	direct, grouped := subscriptionFixture("direct"), subscriptionFixture("grouped")
	grouped.Debounce = debounceFixture("subject")
	catalog, err := CompileCatalog("team", "catalog-v1", []Subscription{direct, grouped})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"specversion":"1.0","id":"one","source":"urn:repo","type":"pr.updated","data":{"gaggle":"foreign","workflow":"privileged"}}`)
	_, plan, err := catalog.Match("team", raw)
	if err != nil || len(plan.Routes) != 2 {
		t.Fatal(plan, err)
	}
	if plan.Routes[0].Workflow != "review" || plan.Routes[0].ConfigGeneration != "generation-pin" || plan.Routes[0].FailureReason != "" || plan.Routes[1].FailureReason != "required debounce attribute is missing" || plan.Routes[1].Debounce != nil {
		t.Fatalf("routing changed authority or made an empty bucket: %+v", plan)
	}
	if _, _, err = catalog.Match("foreign", raw); err == nil {
		t.Fatal("cross-gaggle catalog used")
	}
	_, unmatched, err := catalog.Match("team", []byte(strings.Replace(string(raw), "pr.updated", "unmatched", 1)))
	if err != nil || unmatched.Routes == nil || len(unmatched.Routes) != 0 {
		t.Fatal("no-match was not successful", unmatched, err)
	}
	if _, err = plan.Marshal(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogFreezesDefinitionsAndFramesDebounceKeys(t *testing.T) {
	sub := subscriptionFixture("grouped")
	sub.Debounce = debounceFixture("subject")
	sub.Filter = Filter{All: []Filter{{Attribute: "type", Equals: "pr.updated"}, {Not: &Filter{Attribute: "source", Equals: "urn:excluded"}}}}
	constant := subscriptionFixture("constant")
	constant.Debounce = debounceFixture("")
	constant.Debounce.ConstantKey = "same"
	defs := []Subscription{sub, constant}
	catalog, err := CompileCatalog("team", "catalog-v1", defs)
	if err != nil {
		t.Fatal(err)
	}
	defs[0].Target.Workflow = "changed"
	defs[0].Debounce.InputMode = "all"
	defs[0].Filter.All[0].Equals = "changed"
	raw := []byte(`{"specversion":"1.0","id":"one","source":"urn:repo","type":"pr.updated","subject":"same"}`)
	_, plan, err := catalog.Match("team", raw)
	if err != nil || len(plan.Routes) != 2 {
		t.Fatal(plan, err)
	}
	if plan.Routes[0].Workflow != "review" || plan.Routes[0].Debounce.InputMode != "latest" || plan.Routes[0].Debounce.Key == plan.Routes[1].Debounce.Key {
		t.Fatal("mutable snapshot or ambiguous key", plan)
	}
	first := plan.Routes[0].Debounce.Key
	plan.Routes[0].Debounce.Key = "modified"
	_, again, err := catalog.Match("team", raw)
	if err != nil || again.Routes[0].Debounce.Key != first {
		t.Fatal("returned route mutated catalog", err)
	}
	raw = []byte(strings.Replace(string(raw), `"urn:repo"`, `"urn:excluded"`, 1))
	_, plan, err = catalog.Match("team", raw)
	if err != nil || len(plan.Routes) != 1 || plan.Routes[0].Consumer != "constant" {
		t.Fatal("not expression failed", plan, err)
	}
}

func TestCatalogRejectsInvalidOrUnboundedFilters(t *testing.T) {
	cycle := Filter{}
	cycle.Not = &cycle
	deep := Filter{Attribute: "type", Equals: "pr.updated"}
	for range 10 {
		copy := deep
		deep = Filter{Not: &copy}
	}
	large := Filter{All: make([]Filter, 32)}
	for i := range large.All {
		large.All[i] = Filter{Any: []Filter{{Attribute: "type", Equals: "pr.updated"}, {Attribute: "type", Equals: "other"}}}
	}
	for _, f := range []Filter{{}, {Attribute: "data.secret", Equals: "x"}, {Attribute: "type", Equals: "x", Not: &Filter{Attribute: "id", Equals: "one"}}, {All: []Filter{}}, cycle, deep, large} {
		sub := subscriptionFixture("bad")
		sub.Filter = f
		if _, err := CompileCatalog("team", "rev", []Subscription{sub}); err == nil {
			t.Fatal("invalid filter accepted")
		}
	}
	for _, policy := range []*DebouncePolicy{debounceFixture("missing"), debounceFixture(""), {KeyAttribute: "subject", ConstantKey: "both", Window: time.Second, MaxWait: time.Minute, MaxEvents: 10, InputMode: "all"}, {KeyAttribute: "subject", Window: time.Nanosecond, MaxWait: time.Minute, MaxEvents: 10, InputMode: "all"}} {
		sub := subscriptionFixture("bad")
		sub.Debounce = policy
		if _, err := CompileCatalog("team", "rev", []Subscription{sub}); err == nil {
			t.Fatal("invalid debounce accepted")
		}
	}
}

func TestCatalogBoundsFanOutAndConcurrentMatching(t *testing.T) {
	defs := make([]Subscription, MaxConsumers+1)
	for i := range defs {
		defs[i] = subscriptionFixture(fmt.Sprintf("consumer-%d", i))
	}
	catalog, err := CompileCatalog("team", "rev", defs)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"specversion":"1.0","id":"one","source":"urn:repo","type":"pr.updated"}`)
	if _, _, err = catalog.Match("team", raw); err == nil {
		t.Fatal("fan-out overflow accepted")
	}
	defs = defs[:MaxConsumers]
	defs[0].Filter = Filter{Any: []Filter{{Attribute: "id", Equals: "one"}, {Attribute: "subject", Equals: "other"}}}
	catalog, err = CompileCatalog("team", "rev", defs)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, plan, err := catalog.Match("team", raw)
			if err != nil || len(plan.Routes) != MaxConsumers {
				t.Errorf("concurrent match: %d %v", len(plan.Routes), err)
			}
		})
	}
	wg.Wait()
}
