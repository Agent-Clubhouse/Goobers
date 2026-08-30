package dispatcher

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// kubePodAPI is the client-go-backed PodAPI — exactly the §4 verb set (pods
// create/delete/get/list; apps/deployments GET only, the DI-9 template read),
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
