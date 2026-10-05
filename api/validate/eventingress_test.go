package validate

import (
	"encoding/json"
	"testing"
)

func TestEventIngressCanonicalClosedSchema(t *testing.T) {
	raw := []byte(`{"apiVersion":"goobers.dev/v1alpha1","kind":"Gaggle","metadata":{"name":"own"},"spec":{"project":{"provider":"github","owner":"acme","name":"app"},"backlog":{"provider":"github","project":"acme/app"},"isolation":{"namespace":"own"},"events":{"ingress":[{"name":"builds","issuer":"https://identity.example","subject":"build-system","source":"urn:builds","allowedTypes":["build.finished"]}]}}}`)
	if err := newV(t).ValidateJSON("gaggle.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	binding := doc["spec"].(map[string]any)["events"].(map[string]any)["ingress"].([]any)[0].(map[string]any)
	binding["gaggle"] = "other"
	raw, _ = json.Marshal(doc)
	if err := newV(t).ValidateJSON("gaggle.schema.json", raw); err == nil {
		t.Fatal("binding accepted alternate gaggle")
	}
}
