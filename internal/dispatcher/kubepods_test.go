package dispatcher

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The client-go PodAPI covers exactly the §4 verb set, with idempotent
// disposal: deleting an absent pod is success (a retried disposal must never
// fail an attempt that already cleaned up).
func TestKubernetesPodAPIVerbs(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gaggle-web", Name: "runner-template"},
	})
	api := NewKubernetesPodAPI(client)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "gaggle-web", Name: "stage-pod",
		Labels: map[string]string{LabelManagedBy: ManagedByValue},
	}}
	if err := api.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	got, err := api.GetPod(ctx, "gaggle-web", "stage-pod")
	if err != nil || got.Name != "stage-pod" {
		t.Fatalf("GetPod = %v %v, want the created pod", got, err)
	}
	listed, err := api.ListPods(ctx, "gaggle-web", map[string]string{LabelManagedBy: ManagedByValue})
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListPods = %v %v, want the labeled pod", listed, err)
	}
	deployment, err := api.GetDeployment(ctx, "gaggle-web", "runner-template")
	if err != nil || deployment.Name != "runner-template" {
		t.Fatalf("GetDeployment = %v %v, want the DI-9 template read", deployment, err)
	}
	if err := api.DeletePod(ctx, "gaggle-web", "stage-pod"); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if err := api.DeletePod(ctx, "gaggle-web", "stage-pod"); err != nil {
		t.Fatalf("deleting an already-absent pod must be a no-op, got %v", err)
	}
	if _, err := api.GetPod(ctx, "gaggle-web", "stage-pod"); err == nil {
		t.Fatal("GetPod after delete must surface the NotFound the supervise loop treats as terminal-unknown")
	}
}
