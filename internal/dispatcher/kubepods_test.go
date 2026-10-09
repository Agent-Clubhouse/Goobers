package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
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

func TestChildFinalizerUsesUIDResourceVersionAndPreservesOthers(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "gaggle", Name: "child", UID: "exact", ResourceVersion: "42", Finalizers: []string{childCustodyFinalizer, "foreign/finalizer"}}}
	client := fake.NewSimpleClientset(p)
	api := NewKubernetesPodAPI(client).(childPodAPI)
	var patches int
	client.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		patches++
		patch := action.(ktesting.PatchAction)
		var ops []map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &ops); err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": "exact"}, {"op": "test", "path": "/metadata/resourceVersion", "value": "42"}, {"op": "replace", "path": "/metadata/finalizers", "value": []any{"foreign/finalizer"}}}
		if patch.GetPatchType() != types.JSONPatchType || !reflect.DeepEqual(ops, want) {
			t.Fatalf("patch=%s", patch.GetPatch())
		}
		return true, p, nil
	})
	if err := api.FinalizePodWithIdentity(t.Context(), p.Namespace, p.Name, "replacement"); !errors.Is(err, ErrChildIsolation) {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatal("patched wrong UID")
	}
	if err := api.FinalizePodWithIdentity(t.Context(), p.Namespace, p.Name, p.UID); err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Fatal(patches)
	}
}
