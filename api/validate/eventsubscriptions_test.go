package validate

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestEventSubscriptionTargetCannotCrossGaggle(t *testing.T) {
	ix := newIndex()
	policy := &apiv1.GaggleEvents{Subscriptions: []apiv1.EventSubscription{{Name: "consumer", Workflow: "review", Filter: apiv1.EventSubscriptionFilter{All: []apiv1.EventAttributeMatch{{Attribute: "type", Equals: "updated"}}}}}}
	ix.gaggles["own"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{Events: policy}}
	ix.workflows[workflowIdentity{gaggle: "other", name: "review"}] = indexedWorkflow{}
	report := &Report{}
	ix.checkEventSubscriptions(report)
	ix.flushReferenceIssues(report)
	if len(report.Issues) != 1 || report.Issues[0].Code != errorEventSubscriptions || !strings.Contains(report.Issues[0].Message, "this gaggle") {
		t.Fatalf("foreign workflow satisfied target: %+v", report.Issues)
	}
	ix.workflows[workflowIdentity{gaggle: "own", name: "review"}] = indexedWorkflow{}
	ix.pendingReferenceIssues = nil
	report = &Report{}
	ix.checkEventSubscriptions(report)
	ix.flushReferenceIssues(report)
	if len(report.Issues) != 0 {
		t.Fatal(report.Issues)
	}
}

func TestEventSubscriptionSchemaAndDeepCopy(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal([]byte(`{"apiVersion":"goobers.dev/v1alpha1","kind":"Gaggle","metadata":{"name":"own"},"spec":{"project":{"provider":"github","owner":"acme","name":"app"},"backlog":{"provider":"github","project":"acme/app"},"isolation":{"namespace":"own"},"events":{"subscriptions":[{"name":"consumer","workflow":"review","filter":{"all":[{"attribute":"type","equals":"pr.updated"}]},"debounce":{"keyAttribute":"subject","maxEvents":2}}]}}}`), &document); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(document)
	if err := newV(t).ValidateJSON("gaggle.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	var gaggle apiv1.Gaggle
	if err := json.Unmarshal(raw, &gaggle); err != nil {
		t.Fatal(err)
	}
	copy := gaggle.DeepCopy()
	copy.Spec.Events.Subscriptions[0].Filter.All[0].Equals = "changed"
	*copy.Spec.Events.Subscriptions[0].Debounce.MaxEvents = 5
	if gaggle.Spec.Events.Subscriptions[0].Filter.All[0].Equals != "pr.updated" || *gaggle.Spec.Events.Subscriptions[0].Debounce.MaxEvents != 2 {
		t.Fatal("deep copy aliases subscription policy")
	}
	events := document["spec"].(map[string]any)["events"].(map[string]any)
	sub := events["subscriptions"].([]any)[0].(map[string]any)
	sub["gaggle"] = "other"
	raw, _ = json.Marshal(document)
	if err := newV(t).ValidateJSON("gaggle.schema.json", raw); err == nil {
		t.Fatal("client-supplied target gaggle accepted")
	}
}

func TestEventPublisherScopeAndDeepCopy(t *testing.T) {
	ix := newIndex()
	policy := &apiv1.GaggleEvents{Publishers: []apiv1.EventPublisher{{Workflow: "produce", AllowedTypes: []string{"build.finished"}}}}
	ix.gaggles["own"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{Events: policy}}
	ix.workflows[workflowIdentity{gaggle: "other", name: "produce"}] = indexedWorkflow{}
	report := &Report{}
	ix.checkEventSubscriptions(report)
	ix.flushReferenceIssues(report)
	if len(report.Issues) != 1 || !strings.Contains(report.Issues[0].Message, "this gaggle") {
		t.Fatal(report.Issues)
	}
	copy := policy.DeepCopy()
	copy.Publishers[0].AllowedTypes[0] = "other"
	if policy.Publishers[0].AllowedTypes[0] != "build.finished" {
		t.Fatal("publisher policy aliases snapshot")
	}
}
