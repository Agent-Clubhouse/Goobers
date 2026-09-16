package main

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
)

func TestEngineStartRevisionPinsGaggleScopeAndPartialClone(t *testing.T) {
	for _, partial := range []bool{false, true} {
		cfg, set, base := runControlsFixture()
		cfg.Workcopies = &instance.WorkcopiesConfig{PartialClone: partial}
		source := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "contributor", Name: base.Name}
		set.Gaggles[0].Spec.AdditionalRepos = []apiv1.RepoRef{source}
		set.Gaggles = append(set.Gaggles, apiv1.Gaggle{
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec: apiv1.GaggleSpec{Project: base, AdditionalRepos: []apiv1.RepoRef{
				{Provider: apiv1.ProviderGitHub, Owner: "not-authorized", Name: "private"},
			}},
		})
		spec, err := engineRunSpec(engineRunRequestFor(t, cfg, set, "web", "implementation"))
		if err != nil {
			t.Fatal(err)
		}
		if spec.PartialClone != partial || !reflect.DeepEqual(spec.AdditionalRepos, []apiv1.RepoRef{source}) {
			t.Fatalf("incorrect configuration pins: partial=%v sources=%+v", spec.PartialClone, spec.AdditionalRepos)
		}
		registry, _, err := bootstrap.RegisterGaggleWorkflows(set, "web")
		if err != nil {
			t.Fatal(err)
		}
		input, err := registry.StartInput("implementation", spec)
		if err != nil {
			t.Fatal(err)
		}
		set.Gaggles[0].Spec.AdditionalRepos[0].Owner = "changed-after-admission"
		if input.PartialClone != partial || !reflect.DeepEqual(input.AdditionalRepos, []apiv1.RepoRef{source}) {
			t.Fatalf("run lost immutable configuration pins: partial=%v sources=%+v", input.PartialClone, input.AdditionalRepos)
		}
	}
}
