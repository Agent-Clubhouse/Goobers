package workflow

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInteractiveFeatureIndependentPreviewContract(t *testing.T) {
	feature, ok := LookupFeature(FeatureInteractive)
	if !ok || feature.Level != SupportPreview || len(feature.DSLVersions) != 1 || feature.DSLVersions[0].Version != "3.0" {
		t.Fatalf("registry missing interactive preview: %+v", feature)
	}
	g := apiv1.Gaggle{DSLVersion: "3.0", Spec: apiv1.GaggleSpec{Interactive: &apiv1.GaggleInteractive{}}}
	diagnostics := CheckInteractiveFeatureSupport(g)
	if len(diagnostics) != 1 || !diagnostics[0].Blocking {
		t.Fatalf("preview was not gated: %+v", diagnostics)
	}
	g.ObjectMeta = metav1.ObjectMeta{Annotations: map[string]string{PreviewFeaturesAnnotation: "true"}}
	diagnostics = CheckInteractiveFeatureSupport(g)
	if len(diagnostics) != 1 || diagnostics[0].Blocking {
		t.Fatalf("own acknowledgement rejected: %+v", diagnostics)
	}
	// Ordinary fields still probe through invoking workflow pins. In particular,
	// a legacy workflow or no workflow must not imply interactive uses DSL 2.0.
	for _, pin := range []string{"2.0", "3.0"} {
		features, err := FeaturesForGaggle(Definition{DSLVersion: pin}, g.Spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range features {
			if f.ID == FeatureInteractive {
				t.Fatal("interactive leaked into workflow-derived feature check")
			}
		}
	}
}
