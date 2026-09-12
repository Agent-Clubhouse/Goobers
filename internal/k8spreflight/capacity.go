package k8spreflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	resourcehelper "k8s.io/component-helpers/resource"
	scheduling "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
)

func checkRunnerClassCapacity(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{ID: "runner-class-capacity", Title: "runner-class pod requests fit a compatible node (incident I-55 / O-11)", Citation: "§7", Severity: SeverityRequired}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list nodes: %v", err)
		return result
	}
	if len(nodes.Items) == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 nodes; capacity is unverified"
		return result
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "goobers.dev/runner-class"})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list runner-class pods: %v", err)
		return result
	}
	classes := map[string]bool{}
	checked := 0
	var offenders []string
	var incomplete bool
	for i := range pods.Items {
		pod := &pods.Items[i]
		class := pod.Labels["goobers.dev/runner-class"]
		if class == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		classes[class] = true
		checked++
		fits, err := runnerPodFitsNodes(pod, nodes.Items)
		if err != nil {
			offenders = append(offenders, pod.Namespace+"/"+pod.Name+": "+err.Error())
		} else if !fits {
			offenders = append(offenders, fmt.Sprintf("%s/%s (class %s) fits no compatible node", pod.Namespace, pod.Name, class))
		}
		incomplete = incomplete || capacityPlacementUnverified(pod)

	}
	if checked == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 runner class(es); no active runner-class pods to inspect"
		return result
	}
	result.Detail = fmt.Sprintf("checked %d runner class(es), %d active pod(s), %d node(s)", len(classes), checked, len(nodes.Items))
	if len(offenders) > 0 {
		sort.Strings(offenders)
		result.Status = StatusFail
		result.Detail += "; " + strings.Join(offenders, "; ")
		result.Hint = "lower per-pod requests or provide a node matching its selectors, affinity, tolerations and resources; replicas are not summed into a single-node request"
		return result
	}
	result.Status = StatusPass
	result.Detail += "; each pod's requests fit one compatible node's allocatable resources"
	result.Hint = "this is static request fit, not current free capacity; volumes, inter-pod placement, and dynamic resource allocation require scheduler observation"
	if incomplete {
		result.Status = StatusWarn
		result.Detail += "; inter-pod placement or dynamic resource constraints remain unverified"
	}
	return result
}

// runnerPodFitsNodes compares against each real node, never a synthetic node
// assembled from independent maximum resources across the cluster.
func runnerPodFitsNodes(pod *corev1.Pod, nodes []corev1.Node) (bool, error) {
	// Authoritative accounting includes app sum, init peak, restartable init
	// sidecars, pod-level budgets, and RuntimeClass overhead.
	requests := resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})
	required := nodeaffinity.GetRequiredNodeAffinity(pod)
	for i := range nodes {
		node := &nodes[i]
		if !capacityNodeEligible(pod, node) {
			continue
		}
		matches, err := required.Match(node)
		if err != nil {
			return false, fmt.Errorf("invalid node affinity: %w", err)
		}
		if matches && capacityResourcesFit(requests, node.Status.Allocatable) {
			return true, nil
		}
	}
	return false, nil
}

func capacityNodeEligible(pod *corev1.Pod, node *corev1.Node) bool {
	if pod.Spec.NodeName != "" && pod.Spec.NodeName != node.Name {
		return false
	}
	if node.Spec.Unschedulable && pod.Spec.NodeName == "" {
		return false
	}
	_, untolerated := scheduling.FindMatchingUntoleratedTaint(klog.Background(), node.Spec.Taints, pod.Spec.Tolerations, func(t *corev1.Taint) bool {
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	}, false)
	return !untolerated
}

func capacityResourcesFit(requests, allocatable corev1.ResourceList) bool {
	for name, quantity := range requests {
		available := allocatable[name]
		if quantity.Cmp(available) > 0 {
			return false
		}
	}
	return true
}

func capacityPlacementUnverified(pod *corev1.Pod) bool {
	affinity := pod.Spec.Affinity
	return affinity != nil && (affinity.PodAffinity != nil || affinity.PodAntiAffinity != nil) || len(pod.Spec.TopologySpreadConstraints) > 0 || len(pod.Spec.ResourceClaims) > 0
}
