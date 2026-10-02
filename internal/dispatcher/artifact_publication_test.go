package dispatcher

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestRenderPodPinsNamedArtifactAuthority(t *testing.T) {
	attempt := testAttempt()
	attempt.ArtifactPublication = &apiv1.ArtifactPublication{Stage: attempt.Stage, Visit: 17, Slots: []apiv1.ArtifactSlot{{Name: "report"}}}
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	var got apiv1.ArtifactPublication
	for _, entry := range pod.Spec.Containers[0].Env {
		if entry.Name == EnvArtifactPublication {
			if err := json.Unmarshal([]byte(entry.Value), &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(&got, attempt.ArtifactPublication) || !slices.Contains(DispatcherPrivilegedEnv, EnvArtifactPublication) {
		t.Fatalf("publication authority lost or exposed: %+v", got)
	}
	attempt.Env = map[string]string{EnvArtifactPublication: `{"stage":"forged"}`}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err == nil {
		t.Fatal("stage environment overrode publication authority")
	}
}

func TestLegacyPodShadowsInheritedArtifactAuthority(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range pod.Spec.Containers[0].Env {
		if entry.Name == EnvArtifactPublication && entry.Value == "" {
			return
		}
	}
	t.Fatal("legacy pod did not shadow inherited publication authority")
}
