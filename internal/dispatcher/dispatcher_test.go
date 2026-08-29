package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
	created       []string      // every pod name ever created, in order
	createdSpecs  []*corev1.Pod // the pods AS CREATED; survives disposal, which deletes from pods
	deleted       []string
	deployments   map[string]*appsv1.Deployment
	createErr     error
	deleteErrFor  map[string]error // pod name → error DeletePod returns for it
	// unschedulable keeps every GetPod reporting Pending with
	// PodScheduled=False/Unschedulable, which is what a pod no node can accept
	// looks like — it never advances to a terminal phase on its own.
	unschedulable bool
	// recoversAfter, when > 0, lets the pod become schedulable again after that
	// many observations, modelling an autoscaler adding a node.
	recoversAfter int
}

func (f *fakePodAPI) key(namespace, name string) string { return namespace + "/" + name }

func (f *fakePodAPI) CreatePod(_ context.Context, pod *corev1.Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.createdSpecs = append(f.createdSpecs, pod.DeepCopy())
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
	if f.unschedulable && (f.recoversAfter == 0 || f.observations[key] <= f.recoversAfter) {
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: 1 node(s) had untolerated taint(s)",
		}}
		return pod.DeepCopy(), nil
	}
	pod.Status.Conditions = nil
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

// mintOnce is a TokenMinter that records what it was asked for.
type mintOnce struct {
	runIDs []string
	token  string
	err    error
}

func (m *mintOnce) Mint(runID string, _ time.Duration) (string, error) {
	m.runIDs = append(m.runIDs, runID)
	return m.token, m.err
}

func podTokenEnv(pod *corev1.Pod) string {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return ""
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvPodToken {
			return e.Value
		}
	}
	return ""
}

// Regression for #3701: before this, Attempt.PodToken was assigned only in
// tests and Registry.Mint had no non-test caller, so GOOBERS_POD_TOKEN was
// never present on a stage pod and every surrender arrived unauthenticated.
func TestDispatchMintsPodTokenAndStampsIt(t *testing.T) {
	pods := &fakePodAPI{}
	minter := &mintOnce{token: "goobers-pod.minted"}
	cfg := testConfig()
	cfg.TokenMinter = minter
	d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	attempt := testAttempt()
	attempt.PodToken = "" // the dispatcher mints only when one was not supplied
	if _, err := d.Dispatch(context.Background(), attempt, []RunnerSpec{linuxRunner()}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(minter.runIDs) != 1 || minter.runIDs[0] != attempt.RunID {
		t.Fatalf("minter called with %v, want exactly [%s]", minter.runIDs, attempt.RunID)
	}
	if len(pods.createdSpecs) != 1 {
		t.Fatalf("expected exactly one created pod, got %d", len(pods.createdSpecs))
	}
	if got := podTokenEnv(pods.createdSpecs[0]); got != "goobers-pod.minted" {
		t.Fatalf("%s = %q, want the minted token stamped on the pod", EnvPodToken, got)
	}
}

// A mint failure must stop the dispatch. Continuing would create a pod that
// cannot surrender, and the failure would resurface much later as an
// unauthenticated PUT that reads like a daemon fault.
func TestDispatchFailsClosedWhenMintFails(t *testing.T) {
	pods := &fakePodAPI{}
	cfg := testConfig()
	cfg.TokenMinter = &mintOnce{err: errors.New("no key")}
	d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	failing := testAttempt()
	failing.PodToken = ""
	if _, err := d.Dispatch(context.Background(), failing, []RunnerSpec{linuxRunner()}); err == nil {
		t.Fatal("Dispatch must fail when the pod token cannot be minted")
	}
	if len(pods.created) != 0 {
		t.Fatalf("no pod may be created when minting failed, created %v", pods.created)
	}
}

// A caller-supplied token is honoured rather than overwritten — the mint is a
// default, not a policy. This is also what makes the two tests above meaningful:
// testAttempt() ships a token, so an unguarded mint would have looked like it
// worked while never exercising the minter.
func TestDispatchKeepsCallerSuppliedPodToken(t *testing.T) {
	pods := &fakePodAPI{}
	minter := &mintOnce{token: "goobers-pod.minted"}
	cfg := testConfig()
	cfg.TokenMinter = minter
	d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	if _, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(minter.runIDs) != 0 {
		t.Fatalf("minter must not be called when the attempt already carries a token, got %v", minter.runIDs)
	}
}

// A pod no node can accept reaches neither PodSucceeded nor PodFailed, so
// without this the supervise loop polls until the ACTIVITY's deadline expires:
// the stage's whole budget spent, reported as a timeout, and the pod left
// behind because dispose only runs after supervise returns.
//
// MEASURED on a live cluster before this existed: a Windows stage pod sat
// Pending for 20 HOURS (scheduler retrying 236 times) because its toleration
// key did not match the node taint. activeDeadlineSeconds=1500 was set and did
// NOT save it — that deadline is relative to the pod's StartTime, and a pod
// that never schedules never gets one.
func TestDispatchFailsUnschedulablePodAndDisposesIt(t *testing.T) {
	pods := &fakePodAPI{unschedulable: true}
	d, err := New(testConfig(), pods, &recordingRelay{}, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()})
	if !errors.Is(err, ErrPodUnschedulable) {
		t.Fatalf("error = %v, want ErrPodUnschedulable", err)
	}
	// The scheduler's own reason must survive: an untolerated taint reads very
	// differently from a resource request no node can satisfy.
	if !strings.Contains(err.Error(), "untolerated taint") {
		t.Fatalf("error must carry the scheduler's message, got: %v", err)
	}
	// The whole point: the pod does not leak while nothing bounds it.
	if !report.Disposed || len(pods.deleted) != 1 {
		t.Fatalf("report = %+v deleted = %v: an unschedulable pod must still be disposed", report, pods.deleted)
	}
}

// Unschedulable is routinely TRANSIENT while the autoscaler adds a node.
// Failing on first sight would break legitimate scale-up, so the grace must be
// a grace and not a check interval.
func TestDispatchToleratesTransientUnschedulability(t *testing.T) {
	pods := &fakePodAPI{unschedulable: true, recoversAfter: 1}
	d, err := New(testConfig(), pods, &recordingRelay{}, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clock := &fakeClock{}
	d.now = clock.Now
	d.sleep = clock.Sleep

	if _, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()}); err != nil {
		t.Fatalf("a pod that becomes schedulable must not be failed: %v", err)
	}
}

// Report.Image is the placement provenance the engine hands back to the run's
// driver (decision 003, "placement provenance in the dispatch result"): the
// image the stage container was actually created with. It is read off the
// RENDERED pod, not off RunnerSpec.Host, and the deployment case is what
// makes the difference observable — there Host is a Deployment NAME and the
// image comes from that Deployment's pod template.
func TestDispatchReportsTheStageImageItRendered(t *testing.T) {
	t.Run("image host", func(t *testing.T) {
		pods := &fakePodAPI{}
		d, _ := newTestDispatcher(t, testConfig(), pods, nil)
		runner := linuxRunner()
		report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{runner})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if report.Image != runner.Host {
			t.Fatalf("report.Image = %q, want the stage container's image %q", report.Image, runner.Host)
		}
	})

	t.Run("deployment template host", func(t *testing.T) {
		const templateImage = "ghcr.io/consumer/fat:" + fullSha
		template := &appsv1.Deployment{}
		template.Name = "consumer-runner"
		template.Spec.Template.Spec.Containers = []corev1.Container{{Name: "stage", Image: templateImage}}
		pods := &fakePodAPI{deployments: map[string]*appsv1.Deployment{"consumer-runner": template}}
		d, _ := newTestDispatcher(t, testConfig(), pods, nil)
		runner := RunnerSpec{
			Name: "consumer", OS: "linux", HostKind: instance.RunnerHostDeployment, Host: "consumer-runner",
		}
		report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{runner})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if report.Image != templateImage {
			t.Fatalf("report.Image = %q, want the TEMPLATE's stage image %q, not the Deployment name %q",
				report.Image, templateImage, runner.Host)
		}
	})

	// A self resolution creates no pod, so there is no image to report — the
	// engine's provenance block is nil for exactly this case.
	t.Run("self host reports no image", func(t *testing.T) {
		pods := &fakePodAPI{}
		d, _ := newTestDispatcher(t, testConfig(), pods, nil)
		self := RunnerSpec{Name: "self", OS: "linux", HostKind: instance.RunnerHostSelf, Host: "self"}
		report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{self})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if !report.Local || report.Image != "" {
			t.Fatalf("report = %+v, want a local resolution with no image", report)
		}
	})
}
