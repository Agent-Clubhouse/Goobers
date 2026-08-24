package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/goobers/goobers/internal/dispatcher"
)

// fakePodAPI is a minimal in-memory dispatcher.PodAPI for testing
// waitForRunningPod against a controllable pod set, without a real cluster.
// CreatePod/GetPod/GetDeployment are unused by e2ekillinject.go and exist
// only to satisfy the interface.
type fakePodAPI struct {
	listFunc func() []corev1.Pod
	deleted  []string
}

func (f *fakePodAPI) CreatePod(context.Context, *corev1.Pod) error { return nil }
func (f *fakePodAPI) GetPod(context.Context, string, string) (*corev1.Pod, error) {
	return nil, nil
}
func (f *fakePodAPI) DeletePod(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakePodAPI) ListPods(_ context.Context, _ string, selector map[string]string) ([]corev1.Pod, error) {
	var out []corev1.Pod
	for _, pod := range f.listFunc() {
		match := true
		for k, v := range selector {
			if pod.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, pod)
		}
	}
	return out, nil
}
func (f *fakePodAPI) GetDeployment(context.Context, string, string) (*appsv1.Deployment, error) {
	return nil, nil
}

var _ dispatcher.PodAPI = (*fakePodAPI)(nil)

func podWith(name, runID, stage, attempt string, phase corev1.PodPhase, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				dispatcher.LabelRun:     runID,
				dispatcher.LabelStage:   stage,
				dispatcher.LabelAttempt: attempt,
			},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestWaitForRunningPodFindsMatchingRunningPod(t *testing.T) {
	var calls atomic.Int32
	api := &fakePodAPI{listFunc: func() []corev1.Pod {
		n := calls.Add(1)
		if n < 2 {
			return []corev1.Pod{podWith("probe-builtin-1", "run-1", "probe-builtin", "1", corev1.PodPending, "")}
		}
		return []corev1.Pod{podWith("probe-builtin-1", "run-1", "probe-builtin", "1", corev1.PodRunning, "node-a")}
	}}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	got, err := waitForRunningPod(context.Background(), api, "gaggle-ns", map[string]string{dispatcher.LabelRun: "run-1", dispatcher.LabelStage: "probe-builtin"}, "", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRunningPod: %v", err)
	}
	if got.Name != "probe-builtin-1" || got.Spec.NodeName != "node-a" {
		t.Fatalf("got = %+v, want the running pod on node-a", got)
	}
}

func TestWaitForRunningPodExcludesGivenName(t *testing.T) {
	api := &fakePodAPI{listFunc: func() []corev1.Pod {
		return []corev1.Pod{
			podWith("probe-builtin-1", "run-1", "probe-builtin", "1", corev1.PodRunning, "node-a"),
			podWith("probe-builtin-2", "run-1", "probe-builtin", "2", corev1.PodRunning, "node-b"),
		}
	}}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	got, err := waitForRunningPod(context.Background(), api, "gaggle-ns", map[string]string{dispatcher.LabelRun: "run-1", dispatcher.LabelStage: "probe-builtin"}, "probe-builtin-1", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRunningPod: %v", err)
	}
	if got.Name != "probe-builtin-2" {
		t.Fatalf("got = %q, want the successor pod probe-builtin-2 (excluding probe-builtin-1)", got.Name)
	}
}

func TestWaitForRunningPodTimesOutWithClearError(t *testing.T) {
	api := &fakePodAPI{listFunc: func() []corev1.Pod { return nil }}
	origSleep := pollSleep
	pollSleep = func(time.Duration) {}
	t.Cleanup(func() { pollSleep = origSleep })

	_, err := waitForRunningPod(context.Background(), api, "gaggle-ns", map[string]string{dispatcher.LabelRun: "run-1"}, "", 0)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
}

func TestAttemptFromPodBuildsThinStageAttempt(t *testing.T) {
	pod := podWith("probe-builtin-1", "run-1", "probe-builtin", "3", corev1.PodRunning, "node-a")
	attempt := attemptFromPod(pod, "probe-builtin")
	if attempt.ID != "probe-builtin-1" {
		t.Errorf("ID = %q, want the pod name", attempt.ID)
	}
	if attempt.Number != 3 {
		t.Errorf("Number = %d, want 3 (from the LabelAttempt label)", attempt.Number)
	}
	if attempt.Status != "Running" {
		t.Errorf("Status = %q, want %q", attempt.Status, "Running")
	}
	if attempt.Placement == nil || attempt.Placement.Pod != "probe-builtin-1" || attempt.Placement.Node != "node-a" {
		t.Errorf("Placement = %+v, want Pod/Node built from the pod object", attempt.Placement)
	}
}

func TestAttemptFromPodLeavesNumberZeroWhenLabelUnparseable(t *testing.T) {
	pod := podWith("p", "run-1", "probe-builtin", "not-a-number", corev1.PodRunning, "node-a")
	attempt := attemptFromPod(pod, "probe-builtin")
	if attempt.Number != 0 {
		t.Errorf("Number = %d, want 0 for an unparseable label rather than a guess", attempt.Number)
	}
}
