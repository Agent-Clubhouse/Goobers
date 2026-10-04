package validate

import (
	"encoding/json"
	"testing"
)

func TestInteractivePolicySchema(t *testing.T) {
	v := newV(t)
	for _, tc := range []struct {
		name   string
		policy string
		valid  bool
	}{
		{"omitted", "", true}, {"empty grants", `{"humans":{}}`, true},
		{"subject", `{"humans":{"operators":[{"issuer":"https://identity.example","subject":"alice"}]},"actions":["run.intervene"],"sourceWrites":{"mode":"pull-request"}}`, true},
		{"group", `{"humans":{"viewers":[{"issuer":"https://identity.example","group":"team"}]},"actions":["backlog.read"]}`, true},
		{"mixed identity", `{"humans":{"viewers":[{"issuer":"issuer","group":"team","subject":"alice"}]}}`, false},
		{"no identity", `{"humans":{"viewers":[{"issuer":"issuer"}]}}`, false},
		{"wildcard action", `{"humans":{},"actions":["*"]}`, false},
		{"duplicate action", `{"humans":{},"actions":["backlog.read","backlog.read"]}`, false},
		{"direct write", `{"humans":{},"sourceWrites":{"mode":"direct"}}`, false},
		{"unknown policy", `{"humans":{},"connectionRef":"ambient"}`, false},
		{"ADO repo", `{"humans":{},"credentials":{"repositories":[{"repository":{"provider":"ado","owner":"org","project":"p","name":"repo"},"credentialRef":"named"}]}}`, true},
		{"ADO missing project", `{"humans":{},"credentials":{"repositories":[{"repository":{"provider":"ado","owner":"org","name":"repo"},"credentialRef":"named"}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{"apiVersion": "goobers.dev/v1alpha1", "kind": "Gaggle", "metadata": map[string]any{"name": "web"}, "spec": map[string]any{"project": map[string]any{"provider": "github", "owner": "acme", "name": "web"}, "backlog": map[string]any{"provider": "github", "project": "acme/web"}, "isolation": map[string]any{"namespace": "web"}}}
			if tc.policy != "" {
				var policy any
				if err := json.Unmarshal([]byte(tc.policy), &policy); err != nil {
					t.Fatal(err)
				}
				doc["spec"].(map[string]any)["interactiveAccess"] = policy
			}
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			err = v.ValidateJSON("gaggle.schema.json", raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
