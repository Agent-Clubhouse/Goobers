package workerrollout

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var workerDeploymentResource = schema.GroupVersionResource{
	Group: "temporal.io", Version: "v1alpha1", Resource: "workerdeployments",
}

// KubernetesStatusSource reads a Temporal Worker Controller WorkerDeployment.
type KubernetesStatusSource struct {
	client    dynamic.Interface
	namespace string
	name      string
}

// NewInClusterStatusSource creates a status reader using the pod's Kubernetes
// service account.
func NewInClusterStatusSource(namespace, name string) (*KubernetesStatusSource, error) {
	if namespace == "" || name == "" {
		return nil, errors.New("worker rollout: Kubernetes namespace and WorkerDeployment name are required")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("worker rollout: load in-cluster Kubernetes config: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("worker rollout: create Kubernetes client: %w", err)
	}
	return &KubernetesStatusSource{client: client, namespace: namespace, name: name}, nil
}

// Status returns the rollout status fields used by retention reconciliation.
func (s *KubernetesStatusSource) Status(ctx context.Context) (DeploymentStatus, error) {
	object, err := s.client.Resource(workerDeploymentResource).Namespace(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return DeploymentStatus{}, err
	}
	return deploymentStatus(object)
}

func deploymentStatus(object *unstructured.Unstructured) (DeploymentStatus, error) {
	if object == nil {
		return DeploymentStatus{}, errors.New("WorkerDeployment response is empty")
	}
	status := DeploymentStatus{}
	status.Generation, _, _ = unstructured.NestedInt64(object.Object, "metadata", "generation")
	status.ObservedGeneration, _, _ = unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	status.TargetBuildID, _, _ = unstructured.NestedString(object.Object, "status", "targetVersion", "buildID")
	status.CurrentBuildID, _, _ = unstructured.NestedString(object.Object, "status", "currentVersion", "buildID")
	count, _, err := unstructured.NestedInt64(object.Object, "status", "versionCount")
	if err != nil {
		return DeploymentStatus{}, fmt.Errorf("read status.versionCount: %w", err)
	}
	status.VersionCount = int(count)
	targetDeployment, found, err := unstructured.NestedMap(object.Object, "status", "targetVersion", "deployment")
	if err != nil {
		return DeploymentStatus{}, fmt.Errorf("read status.targetVersion.deployment: %w", err)
	}
	status.TargetHasDeployment = found && len(targetDeployment) > 0
	deprecated, found, err := unstructured.NestedSlice(object.Object, "status", "deprecatedVersions")
	if err != nil {
		return DeploymentStatus{}, fmt.Errorf("read status.deprecatedVersions: %w", err)
	}
	if !found {
		return status, nil
	}
	for index, raw := range deprecated {
		version, ok := raw.(map[string]any)
		if !ok {
			return DeploymentStatus{}, fmt.Errorf("status.deprecatedVersions[%d] is not an object", index)
		}
		buildID, _, _ := unstructured.NestedString(version, "buildID")
		versionStatus, _, _ := unstructured.NestedString(version, "status")
		eligible, _, _ := unstructured.NestedBool(version, "eligibleForDeletion")
		status.Deprecated = append(status.Deprecated, DeprecatedVersion{
			BuildID: buildID, Status: versionStatus, EligibleForDeletion: eligible,
		})
	}
	return status, nil
}
