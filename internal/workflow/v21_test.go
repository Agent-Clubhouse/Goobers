package workflow

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestV21InheritsOnlyV20LanguageSurface(t *testing.T) {
	def := commentWatcherDefinition(t, "2.1")
	def.Spec.Tasks[0].RunsOn = &apiv1.RunsOn{OS: "linux"}
	if _, err := Compile(def, WithPreviewFeatures(true)); err == nil || !strings.Contains(err.Error(), "runsOn") {
		t.Fatalf("2.1 admitted 3.x placement: %v", err)
	}
	if _, err := StagePlacements(Definition{DSLVersion: "2.1"}, apiv1.GaggleSpec{RunsOn: &apiv1.GaggleRunsOn{OS: "linux"}}, nil); err == nil {
		t.Fatal("2.1 admitted gaggle runsOn")
	}
}

func TestV21FeatureRegistryPreservesV20Levels(t *testing.T) {
	legacy, err := FeaturesAtDSLVersion(AllFeatures(), "2.0")
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := FeaturesAtDSLVersion(AllFeatures(), "2.1")
	if err != nil {
		t.Fatal(err)
	}
	levels := func(features []Feature) map[FeatureID]SupportLevel {
		out := map[FeatureID]SupportLevel{}
		for _, feature := range features {
			out[feature.ID] = feature.Level
		}
		return out
	}
	if !reflect.DeepEqual(levels(legacy), levels(inherited)) {
		t.Fatal("2.1 feature surface/levels differ from 2.0")
	}
	registry, err := NewFeatureRegistry(Definition{DSLVersion: "2.1"}, inherited)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.All()) != len(inherited) {
		t.Fatal("2.1 registry lost features")
	}
	def := commentWatcherDefinition(t, "2.1")
	used, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range used {
		if !featureDeclaresDSLVersion(feature, "2.1") {
			t.Errorf("used feature %s missing 2.1 support", feature.ID)
		}
	}
	if diagnostics := CheckWorkflowFeatureSupport(def, true); len(diagnostics) != 0 {
		t.Fatalf("feature support: %+v", diagnostics)
	}
	if problems := CheckStageContracts(def); len(problems) != 0 {
		t.Fatalf("stage contracts: %v", problems)
	}
	for _, query := range []func() ([]Feature, error){
		func() ([]Feature, error) {
			return FeaturesForGaggle(def, apiv1.GaggleSpec{RequiredCapabilities: []string{"dotnet@8"}})
		},
		func() ([]Feature, error) { return FeaturesForGoober(def, apiv1.GooberSpec{}) },
	} {
		features, err := query()
		if err != nil {
			t.Fatal(err)
		}
		for _, feature := range features {
			if !featureDeclaresDSLVersion(feature, "2.1") {
				t.Errorf("resource feature %s missing 2.1 support", feature.ID)
			}
		}
	}
}

func TestV21DoesNotChangeDefaultsOrAdmitUnknownPins(t *testing.T) {
	if got, ok := supportmatrix.GetDSL().NewestSupported(); !ok || got != "2.0" {
		t.Fatalf("newest supported = %q, %v", got, ok)
	}
	interpreter, err := interpreterForVersion("")
	if err != nil || interpreter != &v20Interpreter {
		t.Fatalf("programmatic default changed: %v", err)
	}
	for _, version := range []string{"2.2", "2.10", "v2.1"} {
		def := commentWatcherDefinition(t, version)
		if _, err := Compile(def); err == nil {
			t.Fatalf("unknown pin %q admitted", version)
		}
	}
}
