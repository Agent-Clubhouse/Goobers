package runsemantics

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestBarrenUpstream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream []StageProduction
		want     []string
	}{
		{name: "no upstream at all is a genuine empty tick"},
		{
			name:     "one producing upstream substantiates the claim",
			upstream: []StageProduction{{Stage: "lens-a"}, {Stage: "lens-b", Delivered: true}},
		},
		{
			name:     "upstream that all journaled nothing is barren",
			upstream: []StageProduction{{Stage: "lens-a"}, {Stage: "lens-b"}},
			want:     []string{"lens-a", "lens-b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BarrenUpstream(tc.upstream); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BarrenUpstream = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeliveredEvidence(t *testing.T) {
	if DeliveredEvidence(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}) {
		t.Fatal("an empty result envelope delivered evidence")
	}
	if !DeliveredEvidence(apiv1.ResultEnvelope{Outputs: map[string]any{"count": 3}}) {
		t.Fatal("an output-carrying result delivered no evidence")
	}
	if !DeliveredEvidence(apiv1.ResultEnvelope{Artifacts: []apiv1.ArtifactPointer{{Path: "a.json"}}}) {
		t.Fatal("an artifact-carrying result delivered no evidence")
	}
}
