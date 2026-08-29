package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// stateTable answers by run ID; an absent run answers the zero value
// (RunStateIndeterminate), which must LEAVE the pod.
type stateTable map[string]RunState

func (s stateTable) RunState(_ context.Context, attempt PodAttempt) RunState {
	return s[attempt.RunID]
}

// seedStagePod renders and creates one dispatcher-labeled pod for run.
//
// The owning workflow id is run+"-run" rather than run itself: every pod in
// this suite therefore carries a driver that CANNOT be reconstructed from the
// run id, which is the scheduled-run shape and the one a composing resolver
// mis-settles.
func seedStagePod(t *testing.T, cfg Config, pods *fakePodAPI, run, name string) *corev1.Pod {
	t.Helper()
	attempt := testAttempt()
	attempt.RunID = run
	attempt.OwningWorkflowID = run + "-run"
	pod, err := RenderPod(cfg, attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	pod.Name = name
	if err := pods.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	return pod
}

// §8 item 3, the reconcile half, under decision 003's rule: on restart the
// sweep disposes a labeled pod only when its attempt is POSITIVELY settled.
// A live attempt is adopted, and an attempt the resolver could not resolve is
// left alone. (The other half of item 3 — no pod outlives
// activeDeadlineSeconds even if the dispatcher crashes — is stamped per pod
// and asserted in TestRenderPodActiveDeadlineAlwaysOn; the kubelet enforces
// it, so the unit surface is the stamp.)
func TestSweepOrphans(t *testing.T) {
	pods := &fakePodAPI{}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Three dispatcher-labeled pods: live run, terminal run, unresolvable run.
	for run, name := range map[string]string{
		"run-live": "pod-live", "run-done": "pod-done", "run-gone": "pod-gone",
	} {
		seedStagePod(t, testConfig(), pods, run, name)
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
		// run-gone deliberately absent → RunStateIndeterminate (zero value).
	})
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "pod-done" {
		t.Fatalf("deleted %v, want exactly pod-done (the settled attempt)", deleted)
	}
	for _, name := range pods.deleted {
		switch name {
		case "pod-live":
			t.Fatal("sweep deleted pod-live — an attempt whose workflow is still Running must be ADOPTED, not disposed")
		case "pod-gone":
			t.Fatal("sweep deleted pod-gone — an attempt the resolver could not resolve must be LEFT, not disposed")
		case "unrelated":
			t.Fatal("sweep deleted a foreign pod")
		}
	}
}

// The owner scope: a worker sweeps only the pods IT created. Decision 003
// wires SweepOrphans on the worker, and a cluster runs more than one — without
// the scope, worker B's restart lists worker A's stage pods and a resolver
// that cannot see A's attempts disposes A's live work.
func TestSweepOrphansIgnoresAnotherOwnersPods(t *testing.T) {
	pods := &fakePodAPI{}
	mine := testConfig()
	mine.Owner = "goobers-worker-0"
	theirs := testConfig()
	theirs.Owner = "goobers-worker-1"
	seedStagePod(t, mine, pods, "run-mine", "pod-mine")
	seedStagePod(t, theirs, pods, "run-theirs", "pod-theirs")

	d, err := New(mine, pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// BOTH runs answer terminal: only the owner scope can keep the sibling's
	// pod alive here.
	deleted, err := d.SweepOrphans(context.Background(), stateTable{
		"run-mine": RunStateTerminal, "run-theirs": RunStateTerminal,
	})
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "pod-mine" {
		t.Fatalf("deleted %v, want exactly pod-mine — a sweep must not reach another worker's stage pods", deleted)
	}
}

func TestSweepOrphansRequiresResolver(t *testing.T) {
	d, err := New(testConfig(), &fakePodAPI{}, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.SweepOrphans(context.Background(), nil); err == nil {
		t.Fatal("nil RunStates accepted — the sweep would have no basis for disposal")
	}
}

// An ownerless dispatcher stamps no owner label, so its selector would match
// every worker's pods. It must refuse rather than sweep the namespace.
func TestSweepOrphansRequiresOwner(t *testing.T) {
	cfg := testConfig()
	cfg.Owner = ""
	d, err := New(cfg, &fakePodAPI{}, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = d.SweepOrphans(context.Background(), stateTable{})
	if err == nil {
		t.Fatal("an ownerless sweep was accepted — its selector matches every worker's stage pods")
	}
	if !strings.Contains(err.Error(), "Owner") {
		t.Fatalf("refusal %q does not name the missing owner", err)
	}
}

// The identity annotations are what makes a selected pod ADDRESSABLE: the
// labels are sanitized and cannot compose a workflow id. A pod missing ANY of
// them is left in place and named, never disposed on a guess.
//
// AnnotationOwningWorkflowID is in this table for a stronger reason than the
// other two. It names the one execution the resolver describes, so a pod
// without it has no driver to ask about at all; the only alternative to
// leaving it would be to compose an id out of the run and stage, which is the
// lossy address this annotation exists to replace.
func TestSweepOrphansLeavesUnaddressablePod(t *testing.T) {
	for _, annotation := range []string{AnnotationOwningWorkflowID, AnnotationRunID, AnnotationStage} {
		t.Run(annotation, func(t *testing.T) {
			pods := &fakePodAPI{}
			pod := seedStagePod(t, testConfig(), pods, "run-a", "pod-a")
			delete(pods.pods[pods.key(pod.Namespace, pod.Name)].Annotations, annotation)

			d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// alwaysTerminal says "settled" about whatever it is handed,
			// including the zero attempt. So the ONLY thing keeping this pod
			// alive is the sweep declining to ask about a pod it cannot
			// address — which is the property under test. A resolver that
			// answered from a table keyed by run id would have kept the pod
			// for the wrong reason.
			deleted, err := d.SweepOrphans(context.Background(), alwaysTerminal{})
			if len(deleted) != 0 {
				t.Fatalf("deleted %v — a pod missing %s cannot be addressed and must be left in place", deleted, annotation)
			}
			if err == nil || !strings.Contains(err.Error(), "pod-a") {
				t.Fatalf("error %v does not name the unaddressable pod", err)
			}
		})
	}
}

// alwaysTerminal settles every attempt it is asked about, the zero one
// included.
type alwaysTerminal struct{}

func (alwaysTerminal) RunState(context.Context, PodAttempt) RunState { return RunStateTerminal }

// The sweep hands the resolver the VERBATIM attempt identity, not the
// sanitized label values: the resolver describes the owning workflow id, and
// neither it nor "run-2026-08-22-0001" survives sanitizeNameSegment.
func TestSweepOrphansPassesVerbatimAttemptIdentity(t *testing.T) {
	pods := &fakePodAPI{}
	attempt := testAttempt()
	attempt.Number = 3
	// Deliberately an identity the label sanitizer MANGLES. With one that
	// round-trips, a sweep reading the labels back would look correct here and
	// address the wrong workflow in production.
	attempt.RunID = "Run.2026_08_22.0001"
	attempt.Stage = "run.unit_tests"
	// And a driver that is neither the run id nor composable from it: the
	// scheduled-run shape, whose RunID the engine rewrote to a hash. If the
	// sweep ever went back to composing an id, this is the value it could not
	// produce.
	attempt.OwningWorkflowID = "Nightly.E2E-2026-08-22T03:00:00Z-run"
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if err := pods.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var seen []PodAttempt
	if _, err := d.SweepOrphans(context.Background(), recordingStates{seen: &seen}); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("resolver saw %d attempts, want 1", len(seen))
	}
	got := seen[0]
	if got.OwningWorkflowID != attempt.OwningWorkflowID {
		t.Fatalf("resolver saw owning workflow %q, want the verbatim %q — the one id a delete is authorised by must never be reconstructed",
			got.OwningWorkflowID, attempt.OwningWorkflowID)
	}
	if got.RunID != attempt.RunID || got.Stage != attempt.Stage || got.Attempt != attempt.Number {
		t.Fatalf("resolver saw %+v, want the verbatim run %q / stage %q / attempt %d",
			got, attempt.RunID, attempt.Stage, attempt.Number)
	}
	if got.Pod != pod.Name || got.Namespace != pod.Namespace {
		t.Fatalf("resolver saw pod %s/%s, want %s/%s", got.Namespace, got.Pod, pod.Namespace, pod.Name)
	}
}

// recordingStates records what it was asked about and leaves every pod.
type recordingStates struct{ seen *[]PodAttempt }

func (r recordingStates) RunState(_ context.Context, attempt PodAttempt) RunState {
	*r.seen = append(*r.seen, attempt)
	return RunStateIndeterminate
}

// One pod's DeletePod error must not strand the rest of the batch. The sweep
// continues, removes every other settled pod, and returns an aggregated error
// naming the failure (not aborting on the first).
func TestSweepOrphansContinuesPastDeleteFailure(t *testing.T) {
	pods := &fakePodAPI{deleteErrFor: map[string]error{"pod-b": errors.New("apiserver conflict")}}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for run, name := range map[string]string{"run-a": "pod-a", "run-b": "pod-b", "run-c": "pod-c"} {
		seedStagePod(t, testConfig(), pods, run, name)
	}
	// All three attempts settled → all three are orphans; pod-b's delete fails.
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
