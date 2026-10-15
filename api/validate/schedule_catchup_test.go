package validate

import (
	"fmt"
	"testing"
)

func TestScheduleCatchUpAuthoringSchema(t *testing.T) {
	validator := newV(t)
	for _, policy := range []string{"coalesce", "all", "future", ""} {
		t.Run(policy, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"apiVersion":"goobers.dev/v1alpha1","kind":"Workflow","metadata":{"name":"scheduled"},"spec":{"gaggle":"example","scheduleCatchUp":%q,"triggers":[{"type":"schedule","schedule":"@every 1m"}],"start":"build"}}`, policy))
			err := validator.ValidateJSON("workflow.schema.json", raw)
			if (err == nil) != (policy == "coalesce" || policy == "all") {
				t.Fatal(err)
			}
		})
	}
}
