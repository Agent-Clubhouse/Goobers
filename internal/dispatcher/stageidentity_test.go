package dispatcher

import (
	"encoding/json"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestStagePodOSAndServiceAccountGolden(t *testing.T) {
	for _, os := range []string{"linux", "windows"} {
		for _, account := range []string{"", "custom-stage", "default"} {
			for _, template := range []bool{false, true} {
				cfg, attempt, runner := testConfig(), testAttempt(), linuxRunner()
				runner.OS = os
				runner.Restrictions = nil
				cfg.GaggleServiceAccounts = map[string]string{attempt.Gaggle: account}
				var pod *corev1.Pod
				var err error
				if template {
					deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "template-account", DeprecatedServiceAccount: "template-account", OS: &corev1.PodOS{Name: "opposite"}, Containers: []corev1.Container{{Name: "stage", Image: runner.Host}}}}}}
					pod, err = RenderFromTemplate(cfg, attempt, runner, deployment)
					if deployment.Spec.Template.Spec.ServiceAccountName != "template-account" {
						t.Fatal("mutated source template")
					}
				} else {
					pod, err = RenderPod(cfg, attempt, runner)
				}
				if err != nil {
					t.Fatal(err)
				}
				fields := struct {
					OS        *corev1.PodOS `json:"os"`
					Account   string        `json:"serviceAccountName"`
					Automount *bool         `json:"automountServiceAccountToken"`
				}{pod.Spec.OS, pod.Spec.ServiceAccountName, pod.Spec.AutomountServiceAccountToken}
				got, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				wantAccount := account
				if wantAccount == "" {
					wantAccount = "goobers-stage"
				}
				want := `{"os":{"name":"` + os + `"},"serviceAccountName":"` + wantAccount + `","automountServiceAccountToken":false}`
				if string(got) != want || pod.Spec.DeprecatedServiceAccount != "" {
					t.Errorf("os=%s account=%q template=%v: got %s; want %s", os, account, template, got, want)
				}
			}
		}
	}
}
