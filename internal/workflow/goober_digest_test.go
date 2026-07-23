package workflow

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestGooberDigestTracksEffectiveParticipatingGoobers(t *testing.T) {
	def := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	def.Spec.Tasks[0].Type = apiv1.TaskAgentic
	def.Spec.Tasks[0].Goober = "coder"
	def.Spec.Tasks[0].Run = nil

	base := apiv1.GooberSpec{
		Instructions: "instructions.md",
		Skills:       []string{"testing", "go"},
		Model:        "model-a",
		Harness:      apiv1.HarnessCopilot,
		HarnessOptions: map[string]apiextensionsv1.JSON{
			"reasoningEffort": {Raw: []byte(`"high"`)},
		},
	}
	digest := func(spec apiv1.GooberSpec, instructions string) string {
		t.Helper()
		value, err := ComputeGooberDigest(
			def,
			map[string]apiv1.GooberSpec{
				"coder": spec,
				"unused": {
					Instructions: "unused.md",
					Model:        "unrelated",
				},
			},
			map[string]string{
				"coder":  instructions,
				"unused": "unused instructions",
			},
		)
		if err != nil {
			t.Fatalf("compute digest: %v", err)
		}
		if value == "" {
			t.Fatal("empty goober digest")
		}
		return value
	}

	original := digest(base, "original instructions")
	for _, tc := range []struct {
		name         string
		spec         apiv1.GooberSpec
		instructions string
	}{
		{name: "instructions content", spec: base, instructions: "changed instructions"},
		{name: "skills", spec: func() apiv1.GooberSpec {
			spec := base
			spec.Skills = []string{"testing", "go", "security"}
			return spec
		}(), instructions: "original instructions"},
		{name: "model", spec: func() apiv1.GooberSpec {
			spec := base
			spec.Model = "model-b"
			return spec
		}(), instructions: "original instructions"},
		{name: "harness", spec: func() apiv1.GooberSpec {
			spec := base
			spec.Harness = apiv1.Harness("alternate")
			return spec
		}(), instructions: "original instructions"},
		{name: "harness options", spec: func() apiv1.GooberSpec {
			spec := base
			spec.HarnessOptions = map[string]apiextensionsv1.JSON{
				"reasoningEffort": {Raw: []byte(`"low"`)},
			}
			return spec
		}(), instructions: "original instructions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := digest(tc.spec, tc.instructions)
			if changed == original {
				t.Fatalf("goober digest did not change: %s", changed)
			}
		})
	}

	reordered := base
	reordered.Instructions = "renamed.md"
	reordered.Skills = []string{"go", "testing", "go"}
	equivalent := digest(reordered, "original instructions")
	if equivalent != original {
		t.Fatalf("path or set ordering changed goober digest: %s != %s", equivalent, original)
	}
}
