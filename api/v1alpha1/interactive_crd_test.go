package v1alpha1

import (
	"os"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestInteractiveCRDContractAndBounds(t *testing.T) {
	path := "../../config/crd/bases/goobers.dev_gaggles.yaml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if len(root.XValidations) != 1 || root.XValidations[0].Rule != "!has(self.spec.interactive) || (has(self.dslVersion) && self.dslVersion == '3.0')" {
		t.Fatalf("missing explicit independent interactive pin: %+v", root.XValidations)
	}
	interactive := root.Properties["spec"].Properties["interactive"]
	surfaces := interactive.Properties["surfaces"]
	if surfaces.MaxItems == nil || *surfaces.MaxItems != 16 || surfaces.XListType == nil || *surfaces.XListType != "map" || !slices.Equal(surfaces.XListMapKeys, []string{"name"}) {
		t.Fatal("surfaces are not a bounded keyed list")
	}
	s := surfaces.Items.Schema
	for _, field := range []string{"name", "mode", "gooberRef", "executionModes", "defaultExecutionMode", "session"} {
		if !slices.Contains(s.Required, field) {
			t.Errorf("missing required %s", field)
		}
	}
	if len(s.XValidations) != 1 || s.XValidations[0].Rule != "self.defaultExecutionMode in self.executionModes" {
		t.Fatal("default execution mode can bypass allowlist")
	}
	if s.Properties["tools"].MaxItems == nil || *s.Properties["tools"].MaxItems != 7 || len(s.Properties["tools"].XValidations) == 0 {
		t.Fatal("tools are not bounded and allowlisted")
	}
	policy := s.Properties["session"]
	if len(policy.XValidations) != 2 || policy.Properties["maxChildren"].Maximum == nil || *policy.Properties["maxChildren"].Maximum != 8 {
		t.Fatal("session bounds missing")
	}
	if errs := validateCRDForInstall(t, path, nil); len(errs) > 0 {
		t.Fatalf("interactive CRD not installable: %v", errs)
	}
}
