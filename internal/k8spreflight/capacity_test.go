package k8spreflight

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func capacityResources(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
}
func capacityNode(name, cpu, memory, os string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelOSStable: os}}, Status: corev1.NodeStatus{Allocatable: capacityResources(cpu, memory)}}
}
func capacityPod(name, cpu, memory string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gaggle-a", Labels: map[string]string{"goobers.dev/runner-class": "shared"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "stage", Resources: corev1.ResourceRequirements{Requests: capacityResources(cpu, memory)}}}}}
}

func TestCapacityRequiresEachPodToFitOneMatchingNode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects func() []runtime.Object
		want    Status
	}{
		{"replicas do not accumulate", func() []runtime.Object {
			return []runtime.Object{capacityNode("linux", "4", "8Gi", "linux"), capacityPod("a", "2", "2Gi"), capacityPod("b", "2", "2Gi"), capacityPod("c", "2", "2Gi")}
		}, StatusPass},
		{"CPU and RAM must fit same node", func() []runtime.Object {
			return []runtime.Object{capacityNode("cpu", "8", "2Gi", "linux"), capacityNode("memory", "2", "8Gi", "linux"), capacityPod("a", "4", "4Gi")}
		}, StatusFail},
		{"wrong OS cannot supply capacity", func() []runtime.Object {
			p := capacityPod("a", "4", "4Gi")
			p.Spec.NodeSelector = map[string]string{corev1.LabelOSStable: "linux"}
			return []runtime.Object{capacityNode("small", "2", "2Gi", "linux"), capacityNode("large", "8", "8Gi", "windows"), p}
		}, StatusFail},
		{"required affinity filters nodes", func() []runtime.Object {
			p := capacityPod("a", "4", "4Gi")
			p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"windows"}}}}}}}}
			return []runtime.Object{capacityNode("linux", "8", "8Gi", "linux"), p}
		}, StatusFail},
		{"untolerated taint", func() []runtime.Object {
			n := capacityNode("linux", "8", "8Gi", "linux")
			n.Spec.Taints = []corev1.Taint{{Key: "reserved", Effect: corev1.TaintEffectNoSchedule}}
			return []runtime.Object{n, capacityPod("a", "4", "4Gi")}
		}, StatusFail},
		{"tolerated taint", func() []runtime.Object {
			n := capacityNode("linux", "8", "8Gi", "linux")
			n.Spec.Taints = []corev1.Taint{{Key: "reserved", Effect: corev1.TaintEffectNoSchedule}}
			p := capacityPod("a", "4", "4Gi")
			p.Spec.Tolerations = []corev1.Toleration{{Key: "reserved", Operator: corev1.TolerationOpExists}}
			return []runtime.Object{n, p}
		}, StatusPass},
		{"serial init peak not sum", func() []runtime.Object {
			p := capacityPod("a", "1", "1Gi")
			for _, name := range []string{"init-a", "init-b"} {
				p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: capacityResources("8", "8Gi")}})
			}
			return []runtime.Object{capacityNode("linux", "8", "8Gi", "linux"), p}
		}, StatusPass},
		{"restartable init overlaps stage", func() []runtime.Object {
			p := capacityPod("a", "3", "1Gi")
			p.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways), Resources: corev1.ResourceRequirements{Requests: capacityResources("2", "1Gi")}}}
			return []runtime.Object{capacityNode("linux", "4", "8Gi", "linux"), p}
		}, StatusFail},
		{"pod overhead counts", func() []runtime.Object {
			p := capacityPod("a", "4", "1Gi")
			p.Spec.Overhead = capacityResources("1", "1Gi")
			return []runtime.Object{capacityNode("linux", "4", "8Gi", "linux"), p}
		}, StatusFail},
		{"pod budget overrides container budget", func() []runtime.Object {
			p := capacityPod("a", "8", "8Gi")
			p.Spec.Resources = &corev1.ResourceRequirements{Requests: capacityResources("2", "2Gi")}
			return []runtime.Object{capacityNode("linux", "4", "4Gi", "linux"), p}
		}, StatusPass},
		{"no active pod is unverified", func() []runtime.Object {
			p := capacityPod("a", "8", "8Gi")
			p.Status.Phase = corev1.PodSucceeded
			return []runtime.Object{capacityNode("linux", "4", "4Gi", "linux"), p}
		}, StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := checkRunnerClassCapacity(context.Background(), fake.NewClientset(tc.objects()...), Options{})
			if result.Status != tc.want {
				t.Fatalf("got %s: %s; want %s", result.Status, result.Detail, tc.want)
			}
		})
	}
}
