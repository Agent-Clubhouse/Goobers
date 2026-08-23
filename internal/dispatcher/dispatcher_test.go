package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/instance"
)

// fakePodAPI is an in-memory PodAPI. Pods advance toward terminalPhase one
// GetPod at a time (Pending → Running → terminal), so the supervise loop
// observes a realistic phase progression.
type fakePodAPI struct {
	mu            sync.Mutex
	pods          map[string]*corev1.Pod
	observations  map[string]int
	terminalPhase corev1.PodPhase
	created       []string // every pod name ever created, in order
	deleted       []string
	deployments   map[string]*appsv1.Deployment
	createErr     error
	deleteErrFor  map[string]error // pod name → error DeletePod returns for it
}

func (f *fakePodAPI) key(namespace, name string) string { return namespace + "/" + name }

func (f *fakePodAPI) CreatePod(_ context.Context, pod *corev1.Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if f.pods == nil {
		f.pods = map[string]*corev1.Pod{}
		f.observations = map[string]int{}
	}
	key := f.key(pod.Namespace, pod.Name)
	if _, exists := f.pods[key]; exists {
		return fmt.Errorf("pod %s already exists", key)
	}
	copied := pod.DeepCopy()
	copied.Status.Phase = corev1.PodPending
	f.pods[key] = copied
	f.created = append(f.created, pod.Name)
	return nil
}

func (f *fakePodAPI) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(namespace, name)
	pod, ok := f.pods[key]
	if !ok {
		return nil, fmt.Errorf("pod %s not found", key)
	}
	f.observations[key]++
	switch f.observations[key] {
	case 1:
		pod.Status.Phase = corev1.PodPending
	case 2:
		pod.Status.Phase = corev1.PodRunning
	default:
		phase := f.terminalPhase
		if phase == "" {
			phase = corev1.PodSucceeded
		}
		pod.Status.Phase = phase
	}
	return pod.DeepCopy(), nil
}

func (f *fakePodAPI) DeletePod(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteErrFor[name]; err != nil {
		return err
	}
	delete(f.pods, f.key(namespace, name))
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakePodAPI) ListPods(_ context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []corev1.Pod
	for _, pod := range f.pods {
		if pod.Namespace != namespace {
			continue
		}
		matches := true
		for key, want := range selector {
			if pod.Labels[key] != want {
				matches = false
			}
		}
		if matches {
			out = append(out, *pod.DeepCopy())
		}
	}
	return out, nil
}

func (f *fakePodAPI) GetDeployment(_ context.Context, _, name string) (*appsv1.Deployment, error) {
	if d, ok := f.deployments[name]; ok {
		return d.DeepCopy(), nil
	}
	return nil, fmt.Errorf("deployment %s not found", name)
}

// confirmGate is a SurrenderGate with a fixed answer.
type confirmGate struct {
	confirmed bool
	err       error
}

func (g confirmGate) Confirmed(context.Context, Attempt) (bool, error) { return g.confirmed, g.err }

// recordingRelay records liveness relays.
type recordingRelay struct {
	mu     sync.Mutex
	phases []corev1.PodPhase
}

func (r *recordingRelay) RelayLiveness(_ context.Context, _ Attempt, _ string, phase corev1.PodPhase) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases = append(r.phases, phase)
	return nil
}

// §8 item 1: a stage attempt runs in a fresh pod the dispatcher created,
// disposed after output surrender — and no pod serves two attempts: a second
// attempt creates a SECOND pod.
func TestDispatchFreshPodPerAttemptDisposedAfterSurrender(t *testing.T) {
	pods := &fakePodAPI{}
	relay := &recordingRelay{}
	d, err := New(testConfig(), pods, relay, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !report.Disposed || !report.SurrenderConfirmed {
		t.Fatalf("report = %+v: pod must be disposed AFTER surrender confirmation", report)
	}
	if report.Phase != corev1.PodSucceeded {
		t.Fatalf("phase = %q", report.Phase)
	}
	if len(pods.created) != 1 || len(pods.deleted) != 1 || pods.created[0] != pods.deleted[0] {
		t.Fatalf("created %v deleted %v: exactly the one fresh pod must exist and be disposed", pods.created, pods.deleted)
	}
	if len(relay.phases) == 0 {
		t.Fatal("no liveness was relayed to the journal seam during supervision")
	}

	// Attempt 2 of the same stage: a DIFFERENT fresh pod.
	second := testAttempt()
	second.Number = 2
	if _, err := d.Dispatch(context.Background(), second, []RunnerSpec{linuxRunner()}); err != nil {
		t.Fatalf("Dispatch attempt 2: %v", err)
	}
	if len(pods.created) != 2 {
		t.Fatalf("attempt 2 did not create a fresh pod: created %v", pods.created)
	}
	if pods.created[0] == pods.created[1] {
		t.Fatalf("both attempts used pod %q — one pod served two attempts (D1 violation)", pods.created[0])
	}
}

// The disposal gate half: surrender NOT confirmed → the typed error surfaces,
// AND the pod is still disposed (the retry gets a fresh pod, never this one).
func TestDispatchUnconfirmedSurrenderStillDisposes(t *testing.T) {
	pods := &fakePodAPI{}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: false}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
	if !errors.Is(err, ErrSurrenderUnconfirmed) {
		t.Fatalf("got %v, want ErrSurrenderUnconfirmed", err)
	}
	if report.SurrenderConfirmed {
		t.Fatal("report claims surrender was confirmed")
	}
	if !report.Disposed || len(pods.deleted) != 1 {
		t.Fatal("the pod must be disposed even when surrender is unconfirmed — one attempt per pod")
	}
}

// A failed stage pod surfaces ErrStageFailed and is disposed.
func TestDispatchFailedStage(t *testing.T) {
	pods := &fakePodAPI{terminalPhase: corev1.PodFailed}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
	if !errors.Is(err, ErrStageFailed) {
		t.Fatalf("got %v, want ErrStageFailed", err)
	}
	if !report.Disposed {
		t.Fatal("failed pod must still be disposed")
	}
}

// A dispose failure must NOT mask a settled SUCCESS: the confirmed surrender
// is authoritative, so Dispatch returns nil and records the disposal failure
// on the report as a leak signal (Disposed stays false, DisposeErr set) rather
// than fabricating an infra error that would discard the surrendered result
// and burn an infra retry on an already-succeeded (possibly MUTATING) stage.
func TestDispatchDisposeFailureDoesNotMaskSettledSuccess(t *testing.T) {
	pods := &fakePodAPI{deleteErrFor: map[string]error{}}
	relay := &recordingRelay{}
	d, err := New(testConfig(), pods, relay, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	// Arm DeletePod to fail for the one pod this attempt creates. Its name is
	// deterministic from the attempt identity, so precompute it.
	podName := PodName(testAttempt())
	pods.deleteErrFor[podName] = errors.New("apiserver conflict")

	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
	if err != nil {
		t.Fatalf("Dispatch returned %v: a dispose failure must not mask a settled success", err)
	}
	if !report.SurrenderConfirmed || report.Phase != corev1.PodSucceeded {
		t.Fatalf("report = %+v: the settled success must be preserved", report)
	}
	if report.Disposed {
		t.Fatal("report.Disposed must stay false when DeletePod failed — it is the leak signal")
	}
	if report.DisposeErr == nil {
		t.Fatal("report.DisposeErr must record the disposal failure")
	}
}

// A dispose failure must NOT mask a settled FAILURE either: the confirmed
// PodFailed result is authoritative, so ErrStageFailed still surfaces (not a
// dispose-masked infra error) with the disposal failure recorded on the report.
func TestDispatchDisposeFailureDoesNotMaskSettledFailure(t *testing.T) {
	pods := &fakePodAPI{terminalPhase: corev1.PodFailed, deleteErrFor: map[string]error{}}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	podName := PodName(testAttempt())
	pods.deleteErrFor[podName] = errors.New("apiserver conflict")

	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
	if !errors.Is(err, ErrStageFailed) {
		t.Fatalf("got %v, want ErrStageFailed — a dispose failure must not mask the settled failure", err)
	}
	if report.Disposed {
		t.Fatal("report.Disposed must stay false when DeletePod failed")
	}
	if report.DisposeErr == nil {
		t.Fatal("report.DisposeErr must record the disposal failure")
	}
}

// host: self resolves to the local execution path: no pod, no k8s calls.
func TestDispatchSelfHostIsLocal(t *testing.T) {
	pods := &fakePodAPI{}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{{
		Name: "self", OS: "macOS", HostKind: instance.RunnerHostSelf, Host: "self",
	}})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !report.Local || report.Pod != "" {
		t.Fatalf("report = %+v: self must be the local path with no pod", report)
	}
	if len(pods.created) != 0 {
		t.Fatal("self host created a pod")
	}
}

// The decision-009 refusal happens BEFORE any pod exists: a skewed image
// creates nothing.
func TestDispatchSkewRefusalCreatesNoPod(t *testing.T) {
	pods := &fakePodAPI{}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner := linuxRunner()
	runner.Host = "ghcr.io/goobers/goobers-base:" + otherSha
	_, err = d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{runner})
	var skew *SkewError
	if !errors.As(err, &skew) {
		t.Fatalf("got %v, want SkewError", err)
	}
	if len(pods.created) != 0 {
		t.Fatal("a skew-refused dispatch created a pod")
	}
}

// The deployment host kind reads the named template and the template's stage
// image goes through the same skew check.
func TestDispatchDeploymentTemplateSkewChecked(t *testing.T) {
	template := &appsv1.Deployment{}
	template.Name = "consumer-runner"
	template.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: "stage", Image: "ghcr.io/consumer/fat:" + otherSha,
	}}
	pods := &fakePodAPI{deployments: map[string]*appsv1.Deployment{"consumer-runner": template}}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner := RunnerSpec{
		Name: "consumer", OS: "linux", HostKind: instance.RunnerHostDeployment, Host: "consumer-runner",
	}
	_, err = d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{runner})
	var skew *SkewError
	if !errors.As(err, &skew) {
		t.Fatalf("got %v, want SkewError for the template's stage image", err)
	}
}
