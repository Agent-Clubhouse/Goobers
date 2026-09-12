package dispatcher

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestRepoOriginRemainsLiteralAndDispatcherOwned(t *testing.T) {
	for _, template := range []bool{false, true} {
		for _, origin := range []string{"https://forge.example/$(GOOBERS_POD_TOKEN)/$$/", ""} {
			a := testAttempt()
			a.RunContext = map[string]string{executorRepoBaseURLEnv: origin}
			render := func() (*corev1.Pod, error) {
				if !template {
					return RenderPod(testConfig(), a, linuxRunner())
				}
				d := testDeployment()
				d.Spec.Template.Spec.Containers[0].Env = append(d.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: executorRepoBaseURLEnv, Value: "http://wrong.invalid"})
				d.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "ambient"}}}}
				return RenderFromTemplate(testConfig(), a, linuxRunner(), d)
			}
			pod, err := render()
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{executorRepoBaseURLEnv: "http://envfrom.invalid"}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = expandPodEnv(e.Value, env)
			}
			if got := env[executorRepoBaseURLEnv]; got != origin {
				t.Fatalf("template=%t origin = %q, want literal %q", template, got, origin)
			}
			for _, declared := range []map[string]string{{executorRepoBaseURLEnv: "http://override.invalid"}, {"COPIED_ORIGIN": "$(GOOBERS_REPO_BASE_URL)"}} {
				a.Env = declared
				_, err = render()
				var override *ControlEnvOverrideError
				if !errors.As(err, &override) {
					t.Fatalf("template=%t env=%v: error=%v, want reserved origin refusal", template, declared, err)
				}
			}
		}
	}
}
