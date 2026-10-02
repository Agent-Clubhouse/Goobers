package k8spreflight

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
	"strings"
	"testing"
)

func TestPodSecurityAdmissionDryRunPerNamespaceAndOS(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "alpha", Labels: map[string]string{"goobers.dev/gaggle": "alpha", "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.24"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "beta", Labels: map[string]string{"goobers.dev/gaggle": "beta", "pod-security.kubernetes.io/enforce": "baseline"}}},
	)
	calls := map[string]bool{}
	client.PrependReactor("create", "pods", func(action kt.Action) (bool, runtime.Object, error) {
		create := action.(kt.CreateAction)
		if dry := action.(interface{ GetCreateOptions() metav1.CreateOptions }).GetCreateOptions().DryRun; len(dry) != 1 || dry[0] != metav1.DryRunAll {
			t.Fatal("create must be server dry-run")
		}
		pod := create.GetObject().(*corev1.Pod)
		if pod.Spec.OS == nil || pod.Spec.ServiceAccountName != "custom-stage" || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Fatalf("unexpected pod: %+v", pod.Spec)
		}
		os := string(pod.Spec.OS.Name)
		calls[action.GetNamespace()+"/"+os] = true
		if os == "linux" && (pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot) {
			t.Fatal("missing Linux security stamp")
		}
		if os == "windows" && pod.Spec.SecurityContext.WindowsOptions == nil {
			t.Fatal("missing Windows security stamp")
		}
		if action.GetNamespace() == "alpha" && os == "windows" {
			return true, nil, fmt.Errorf("violates PodSecurity restricted:v1.24")
		}
		return true, pod, nil
	})
	report := Run(context.Background(), client, Options{Checks: []string{"pod-security-admission"}, PSAServiceAccount: "custom-stage"})
	if !report.Conformant {
		t.Fatal("informational admission blocked conformance")
	}
	result := report.Results[0]
	if result.Status != StatusWarn || len(calls) != 4 {
		t.Fatalf("result=%+v calls=%v", result, calls)
	}
	for _, want := range []string{"alpha (enforce=restricted:v1.24), windows: not admitted", "beta (enforce=baseline), windows: accepted", "alpha (enforce=restricted:v1.24), linux: accepted"} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("missing %q in %s", want, result.Detail)
		}
	}
	pods, err := client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 0 {
		t.Fatalf("probe persisted pods: %v %v", pods, err)
	}
}

func TestPodSecurityAdmissionUncheckedAndExplicitSelection(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "explicit"}})
	if r := checkPodSecurityAdmission(context.Background(), client, Options{}); r.Status != StatusWarn || !strings.Contains(r.Detail, "unchecked") {
		t.Fatalf("%+v", r)
	}
	calls := 0
	client.PrependReactor("create", "pods", func(action kt.Action) (bool, runtime.Object, error) {
		calls++
		return true, action.(kt.CreateAction).GetObject(), nil
	})
	r := checkPodSecurityAdmission(context.Background(), client, Options{PSANamespaces: []string{"explicit", "explicit", "missing"}})
	if calls != 2 || r.Status != StatusWarn || !strings.Contains(r.Detail, "missing: unchecked:") {
		t.Fatalf("calls=%d result=%+v", calls, r)
	}
}
