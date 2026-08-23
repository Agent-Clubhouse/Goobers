package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

type stateTable map[string]RunState

func (s stateTable) RunState(_ context.Context, runLabel string) RunState { return s[runLabel] }

// §8 item 3, the reconcile half: on restart the sweep deletes any labeled pod
// whose run is terminal or UNKNOWN, and keeps pods of live runs. (The other
// half of item 3 — no pod outlives activeDeadlineSeconds even if the
// dispatcher crashes — is stamped per pod and asserted in
// TestRenderPodActiveDeadlineAlwaysOn; the kubelet enforces it, so the unit
// surface is the stamp.)
func TestSweepOrphans(t *testing.T) {
	pods := &fakePodAPI{}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Three dispatcher-labeled pods: live run, terminal run, unknown run.
	for run, name := range map[string]string{
		"run-live": "pod-live", "run-done": "pod-done", "run-gone": "pod-gone",
	} {
		attempt := testAttempt()
		attempt.RunID = run
		pod, err := RenderPod(testConfig(), attempt, linuxRunner())
		if err != nil {
			t.Fatalf("RenderPod: %v", err)
		}
		pod.Name = name
		if err := pods.CreatePod(context.Background(), pod); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
	}
	// A foreign pod in the namespace: not dispatcher-labeled, never touched.
	foreign := &corev1.Pod{}
	foreign.Name = "unrelated"
	foreign.Namespace = testConfig().Namespace
	if err := pods.CreatePod(context.Background(), foreign); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	deleted, err := d.SweepOrphans(context.Background(), stateTable{
		"run-live": RunStateLive,
		"run-done": RunStateTerminal,
		// run-gone deliberately absent → RunStateUnknown (zero value).
	})
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	got := map[string]bool{}
	for _, name := range deleted {
		got[name] = true
	}
	if !got["pod-done"] || !got["pod-gone"] || len(got) != 2 {
		t.Fatalf("deleted %v, want exactly pod-done (terminal) and pod-gone (unknown)", deleted)
	}
	for _, name := range pods.deleted {
		if name == "pod-live" || name == "unrelated" {
			t.Fatalf("sweep deleted %q — live-run pods and foreign pods must survive", name)
		}
	}
}

func TestSweepOrphansRequiresResolver(t *testing.T) {
	d, err := New(testConfig(), &fakePodAPI{}, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.SweepOrphans(context.Background(), nil); err == nil {
		t.Fatal("nil RunStates accepted — the sweep would delete everything it cannot resolve")
	}
}

// Fail CLOSED toward cleanup: one pod's DeletePod error must not strand the
// rest of the batch. The sweep continues, removes every other orphan, and
// returns an aggregated error naming the failure (not aborting on the first).
func TestSweepOrphansContinuesPastDeleteFailure(t *testing.T) {
	pods := &fakePodAPI{deleteErrFor: map[string]error{"pod-b": errors.New("apiserver conflict")}}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for run, name := range map[string]string{"run-a": "pod-a", "run-b": "pod-b", "run-c": "pod-c"} {
		attempt := testAttempt()
		attempt.RunID = run
		pod, err := RenderPod(testConfig(), attempt, linuxRunner())
		if err != nil {
			t.Fatalf("RenderPod: %v", err)
		}
		pod.Name = name
		if err := pods.CreatePod(context.Background(), pod); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
	}
	// All three runs terminal → all three are orphans; pod-b's delete fails.
	deleted, err := d.SweepOrphans(context.Background(), stateTable{
		"run-a": RunStateTerminal, "run-b": RunStateTerminal, "run-c": RunStateTerminal,
	})
	if err == nil {
		t.Fatal("expected an aggregated error naming the failed delete, got nil")
	}
	if !strings.Contains(err.Error(), "pod-b") {
		t.Fatalf("aggregated error %q does not name the failed pod pod-b", err)
	}
	got := map[string]bool{}
	for _, name := range deleted {
		got[name] = true
	}
	if !got["pod-a"] || !got["pod-c"] {
		t.Fatalf("deleted %v — the other orphans must be removed despite pod-b failing", deleted)
	}
	if got["pod-b"] {
		t.Fatal("pod-b reported deleted despite its DeletePod erroring")
	}
}
