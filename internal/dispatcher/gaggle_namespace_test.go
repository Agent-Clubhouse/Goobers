package dispatcher

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// templateDeployment is the minimal DI-9 template deployment RenderFromTemplate
// needs: one non-empty container to become the stage container.
func templateDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "stage", Image: "stage:test"}}},
	}}}
}

// #4897: each gaggle's stage pods must land in ITS OWN declared namespace, two
// distinct gaggles sharing one worker never fall back to a single process-wide
// namespace, and no gaggle absent from Config.GaggleNamespaces is guessed at.

func TestNewRequiresGaggleNamespaces(t *testing.T) {
	cfg := testConfig()
	cfg.GaggleNamespaces = nil
	if _, err := New(cfg, &fakePodAPI{}, nil, confirmGate{confirmed: true}, nil); err == nil {
		t.Fatal("New accepted a Config with no declared gaggle namespaces")
	}
}

func TestNewRejectsEmptyDeclaredNamespace(t *testing.T) {
	cfg := testConfig()
	cfg.GaggleNamespaces = map[string]string{"alpha": ""}
	if _, err := New(cfg, &fakePodAPI{}, nil, confirmGate{confirmed: true}, nil); err == nil {
		t.Fatal("New accepted a gaggle mapped to an empty namespace")
	}
}

// Two gaggles this worker serves, with DISTINCT declared namespaces, must
// produce pods in their own namespaces — never the other gaggle's, and never
// a single shared worker-wide value (the exact break #4897 closes).
func TestRenderPodRoutesEachGaggleToItsOwnDeclaredNamespace(t *testing.T) {
	cfg := testConfig()
	cfg.GaggleNamespaces = map[string]string{
		"alpha": "gaggle-alpha",
		"beta":  "gaggle-beta",
	}
	for _, tc := range []struct{ gaggle, wantNamespace string }{
		{"alpha", "gaggle-alpha"},
		{"beta", "gaggle-beta"},
	} {
		attempt := testAttempt()
		attempt.Gaggle = tc.gaggle
		pod, err := RenderPod(cfg, attempt, linuxRunner())
		if err != nil {
			t.Fatalf("RenderPod(%s): %v", tc.gaggle, err)
		}
		if pod.Namespace != tc.wantNamespace {
			t.Fatalf("gaggle %s pod namespace = %q, want %q", tc.gaggle, pod.Namespace, tc.wantNamespace)
		}
	}
}

// The supported shared topology (#4897 maintainer contract): two gaggles may
// EXPLICITLY declare the same namespace, and both dispatch into it without
// either being refused or silently rehomed.
func TestRenderPodPreservesExplicitSharedNamespaceTopology(t *testing.T) {
	cfg := testConfig()
	cfg.GaggleNamespaces = map[string]string{
		"alpha": "shared-gaggle-ns",
		"beta":  "shared-gaggle-ns",
	}
	for _, gaggle := range []string{"alpha", "beta"} {
		attempt := testAttempt()
		attempt.Gaggle = gaggle
		pod, err := RenderPod(cfg, attempt, linuxRunner())
		if err != nil {
			t.Fatalf("RenderPod(%s): %v", gaggle, err)
		}
		if pod.Namespace != "shared-gaggle-ns" {
			t.Fatalf("gaggle %s pod namespace = %q, want the shared namespace", gaggle, pod.Namespace)
		}
	}
}

// An attempt whose gaggle has no entry in Config.GaggleNamespaces must be
// refused, never guessed into some other namespace.
func TestRenderPodRefusesAttemptForUndeclaredGaggle(t *testing.T) {
	cfg := testConfig()
	attempt := testAttempt()
	attempt.Gaggle = "not-declared"
	if _, err := RenderPod(cfg, attempt, linuxRunner()); err == nil {
		t.Fatal("RenderPod accepted an attempt for a gaggle with no declared namespace")
	} else if !strings.Contains(err.Error(), "not-declared") {
		t.Fatalf("error %v does not name the undeclared gaggle", err)
	}
}

func TestRenderFromTemplateRoutesEachGaggleToItsOwnDeclaredNamespace(t *testing.T) {
	cfg := testConfig()
	cfg.GaggleNamespaces = map[string]string{
		"alpha": "gaggle-alpha",
		"beta":  "gaggle-beta",
	}
	deployment := templateDeployment()
	for _, tc := range []struct{ gaggle, wantNamespace string }{
		{"alpha", "gaggle-alpha"},
		{"beta", "gaggle-beta"},
	} {
		attempt := testAttempt()
		attempt.Gaggle = tc.gaggle
		pod, err := RenderFromTemplate(cfg, attempt, linuxRunner(), deployment)
		if err != nil {
			t.Fatalf("RenderFromTemplate(%s): %v", tc.gaggle, err)
		}
		if pod.Namespace != tc.wantNamespace {
			t.Fatalf("gaggle %s pod namespace = %q, want %q", tc.gaggle, pod.Namespace, tc.wantNamespace)
		}
	}
}

// A restart's reconcile sweep must reach every namespace this worker
// dispatches into, not just the first gaggle's — otherwise a second gaggle's
// settled stage pods are stranded forever once #4897 stops sharing one
// process-wide namespace.
func TestSweepOrphansCoversEveryDeclaredGaggleNamespace(t *testing.T) {
	pods := &fakePodAPI{}
	cfg := testConfig()
	cfg.GaggleNamespaces = map[string]string{
		"alpha": "gaggle-alpha",
		"beta":  "gaggle-beta",
	}
	for gaggle, run := range map[string]string{"alpha": "run-alpha", "beta": "run-beta"} {
		attempt := testAttempt()
		attempt.Gaggle = gaggle
		attempt.RunID = run
		attempt.OwningWorkflowID = run + "-run"
		pod, err := RenderPod(cfg, attempt, linuxRunner())
		if err != nil {
			t.Fatalf("RenderPod(%s): %v", gaggle, err)
		}
		pod.Name = "pod-" + gaggle
		if err := pods.CreatePod(context.Background(), pod); err != nil {
			t.Fatalf("CreatePod(%s): %v", gaggle, err)
		}
	}
	d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	deleted, err := d.SweepOrphans(context.Background(), stateTable{
		"run-alpha": RunStateTerminal, "run-beta": RunStateTerminal,
	})
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	got := map[string]bool{}
	for _, name := range deleted {
		got[name] = true
	}
	if !got["pod-alpha"] || !got["pod-beta"] {
		t.Fatalf("deleted %v, want both gaggles' settled pods swept across their own namespaces", deleted)
	}
}
