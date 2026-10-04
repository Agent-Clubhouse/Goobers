package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// kubePodAPI is the client-go-backed PodAPI — exactly the §4 verb set (pods
// create/delete/get/list and custody-finalizer patch; apps/deployments GET only,
// the DI-9 template read;
// persistentvolumeclaims GET only, the #5595 module cache claim check),
// nothing wider, so the dispatcher RBAC stays the narrow Role the design
// renders.
type kubePodAPI struct {
	client kubernetes.Interface
}

// NewKubernetesPodAPI wraps a typed clientset as the dispatcher's PodAPI.
func NewKubernetesPodAPI(client kubernetes.Interface) PodAPI {
	return &kubePodAPI{client: client}
}

// CreatePod creates the pod in its manifest's namespace.
func (k *kubePodAPI) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	_, err := k.client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

// CreatePodWithIdentity returns the atomic API creation receipt for child pods.
func (k *kubePodAPI) CreatePodWithIdentity(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return k.client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
}

// DeletePodWithIdentity never deletes a replacement that reused the same name.
func (k *kubePodAPI) DeletePodWithIdentity(ctx context.Context, namespace, name string, uid types.UID) error {
	err := k.client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// FinalizePodWithIdentity releases only our custody finalizer after the caller
// verified termination and surrender. UID and resourceVersion tests prevent
// replacement/deletion races, and every unrelated finalizer remains intact.
func (k *kubePodAPI) FinalizePodWithIdentity(ctx context.Context, namespace, name string, uid types.UID) error {
	pod, err := k.GetPod(ctx, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil // caller already holds exact-object terminal evidence
	}
	if err != nil {
		return err
	}
	if pod.UID != uid || pod.ResourceVersion == "" {
		return ErrChildIsolation
	}
	finalizers := slices.DeleteFunc(slices.Clone(pod.Finalizers), func(value string) bool { return value == childCustodyFinalizer })
	if len(finalizers) == len(pod.Finalizers) {
		return nil
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": string(uid)},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
		{"op": "replace", "path": "/metadata/finalizers", "value": finalizers},
	})
	if err != nil {
		return err
	}
	_, err = k.client.CoreV1().Pods(namespace).Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{})
	return err
}

// GetPod reads one pod; a NotFound surfaces as the error the supervise loop
// treats as terminal-unknown.
func (k *kubePodAPI) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return k.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

// DeletePod deletes one pod. Deleting an already-absent pod is a no-op —
// disposal is idempotent by contract.
func (k *kubePodAPI) DeletePod(ctx context.Context, namespace, name string) error {
	err := k.client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ListPods lists pods matching every label in selector.
func (k *kubePodAPI) ListPods(ctx context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error) {
	list, err := k.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(selector).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("dispatcher: list pods in %s: %w", namespace, err)
	}
	return list.Items, nil
}

// GetDeployment reads a consumer-authored Deployment used as a pod template
// by reference (DI-9).
func (k *kubePodAPI) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return k.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

// GetPersistentVolumeClaim reads one claim — the #5595 check for the durable
// Go module cache claim. A Forbidden here is not fatal: the dispatcher falls
// back to an emptyDir cache, so a Role without this verb still dispatches.
func (k *kubePodAPI) GetPersistentVolumeClaim(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	return k.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
}

// GetService resolves stable ClusterIPs for network:none host aliases.
func (k *kubePodAPI) GetService(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	return k.client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}
