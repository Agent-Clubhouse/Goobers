package dispatcher

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPhysicalPodIdentityPreservesJournalAndOwner(t *testing.T) {
	for _, physical := range []int{0, 7} {
		a := testAttempt()
		a.Number = 1
		a.PodAttempt = physical
		a.OwningWorkflowID = "actual-owner-workflow"
		cfg := Config{Namespace: "test"}
		for _, template := range []bool{false, true} {
			var pod *corev1.Pod
			var err error
			if template {
				pod, err = RenderFromTemplate(cfg, a, linuxRunner(), &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelPodAttempt: "999"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "stage", Image: "stage:test", EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "consumer-env"}}}}, Env: []corev1.EnvVar{{Name: EnvPodAttempt, Value: "999"}}}}}}}})
			} else {
				pod, err = RenderPod(cfg, a, linuxRunner())
			}
			if err != nil {
				t.Fatal(err)
			}
			id, ok := podAttempt(pod)
			if !ok || id.OwningWorkflowID != a.OwningWorkflowID || id.Attempt != 1 {
				t.Fatalf("cleanup identity changed: %+v", id)
			}
			if pod.Labels[LabelAttempt] != "1" {
				t.Fatal("journal attempt label changed")
			}
			if physical == 0 && pod.Labels[LabelPodAttempt] != "" {
				t.Fatal("legacy template forged physical label")
			}
			if physical > 0 && pod.Labels[LabelPodAttempt] != "7" {
				t.Fatal("physical label not stamped")
			}
			if template && len(pod.Spec.Containers[0].EnvFrom) != 1 {
				t.Fatal("consumer envFrom removed")
			}
			seenPhysical := 0
			for _, env := range pod.Spec.Containers[0].Env {
				if env.Name == EnvAttempt && env.Value != "1" {
					t.Fatal("invocation ordinal changed")
				}
				if env.Name == EnvPodAttempt {
					seenPhysical++
					if (physical == 0 && env.Value != "") || (physical > 0 && env.Value != "7") {
						t.Fatalf("template supplied physical identity: %+v", env)
					}
				}
			}
			if seenPhysical != 1 {
				t.Fatalf("physical env count=%d", seenPhysical)
			}
		}
		plane, err := NewSurrenderDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := plane.Put(context.Background(), a.RunID, a.Stage, 1, []byte(`{"result":{"status":"success"}}`)); err != nil {
			t.Fatal(err)
		}
		confirmed, err := (PlaneSurrenderGate{Plane: plane}).Confirmed(context.Background(), a)
		if err != nil || confirmed != (physical == 0) {
			t.Fatalf("old surrender confirmed physical=%d: %t/%v", physical, confirmed, err)
		}
	}
}
