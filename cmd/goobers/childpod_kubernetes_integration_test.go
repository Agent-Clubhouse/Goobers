//go:build integration

package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	k8sptr "k8s.io/utils/ptr"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This opt-in test uses an actual Temporal server and Kubernetes dispatcher.
// Only the originating parent stage is a fixture; no public parent admission
// guard is bypassed. Run only against the disposable kind cluster described in
// the transport guide, with a freshly built Goobers worker image.
func TestIntegrationQueuedChildUsesRealKubernetesWorker(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	testdep.Require(t, "git")
	image := os.Getenv("GOOBERS_CHILD_QUALIFICATION_IMAGE")
	if !strings.HasPrefix(image, "localhost:45081/goobers:") {
		t.Fatal("qualification requires the task-local image registry")
	}
	api, namespace := childQualificationKubernetes(t)
	source := strings.Replace(childValidationProposal, "      run: {command: [\"true\"]}", "      timeoutSeconds: 60\n      runsOn: {os: linux, capabilities: [isolated-child]}\n      run: {workspace: scratch, command: [sh, -c, 'test $$ -gt 1; echo real-contained-worker']}", 1)
	keyPath := filepath.Join(t.TempDir(), "pod.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("q", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	f := newChildKitFixtureConfigured(t, childKitFixtureOptions{isolated: true, queued: true, runnerImage: image, runnerShell: true, podTokenKeyFile: keyPath, source: source})
	s := f.writer.service
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.log, _, err = journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.log.Close() })
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("q", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := temporaltest.StartDevServer(t.Context(), t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dev.Stop() })
	transport := &observedChildTemporal{Client: dev.Client()}
	journals := childFactoryJournals(t, s)
	observe := childObservationFunc(func(context.Context, httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
		return httpapi.ClaimListResponse{ClaimVisibility: "local", ObservedAt: time.Now()}, nil
	})
	opts := s.installChildPodPlane(transport, plane, journals, observe, nil)
	opts = append(opts, httpapi.WithAuthenticator(auth.WithChildWorkflowGrants(s.grants.key)), httpapi.WithChildWorkflowService(s.children.HTTPService()))
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + net.JoinHostPort("host.docker.internal", address.Port())
	dispatch, err := dispatcher.New(dispatcher.Config{InstanceID: f.writer.identity.InstanceID, Owner: "local-qualification", GaggleNamespaces: map[string]string{"example": namespace}, GaggleServiceAccounts: map[string]string{"example": "qualification-stage"}, EmbeddedVersion: strings.TrimPrefix(image, "localhost:45081/goobers:"), TokenMinter: key, BlobEndpoint: endpoint, WriteAPIBase: endpoint, SupervisionInterval: 100 * time.Millisecond, LinuxScheduleToStart: 30 * time.Second}, dispatcher.NewKubernetesPodAPI(api), nil, dispatcher.PlaneSurrenderGate{Plane: plane}, nil)
	if err != nil {
		t.Fatal(err)
	}
	launcher := &queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}
	receipt, err := s.childQueue.ChildStart(t.Context(), f.child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := (&durableTriggerService{queue: s.childQueue}).childReference(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	authority, release, err := launcher.acquire(t.Context(), ref.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := s.childQueue.ChildProposal(t.Context(), f.child.Identity)
	if err != nil {
		release()
		t.Fatal(err)
	}
	proposal, err := childworkflow.ValidateRetainedStart(authority, ref.Envelope, retained.Source)
	release()
	if err != nil {
		t.Fatal(err)
	}
	queues := map[string]bool{s.config.EffectiveEngineConfig().TaskQueue: true}
	for _, pin := range proposal.Placements {
		queues[pin.Queue] = true
	}
	for queue := range queues {
		worker := temporalworker.New(dev.Client(), queue, temporalworker.Options{DeadlockDetectionTimeout: temporaltest.DeadlockDetectionTimeout, WorkerStopTimeout: time.Second})
		engine.RegisterWith(worker, &engine.Activities{Dispatcher: dispatch, Surrenders: plane})
		if err := worker.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(worker.Stop)
	}
	finish := prepareQueuedParentReturn(t, f, server.URL)
	drainRealQueuedChild(t, f)
	calls := transport.snapshot()
	if len(calls) != 1 {
		t.Fatalf("actual worker starts = %d, want 1", len(calls))
	}
	for id, in := range calls {
		var result engine.ChildDispatchResult
		if err := dev.Client().GetWorkflow(t.Context(), id, "").Get(t.Context(), &result); err != nil {
			t.Fatal(err)
		}
		report := result.Report
		if result.BindingDigest != in.BindingDigest() || result.DispatchError() != nil || result.DisposalFailed || report.ChildPodUID == "" || !report.WorkspaceWritersStopped || !report.SurrenderConfirmed {
			t.Fatalf("real worker lacks exact stopped-writer custody: %+v", result)
		}
		t.Logf("real pod %s UID %s: stopped=%t surrendered=%t", report.Pod, report.ChildPodUID, report.WorkspaceWritersStopped, report.SurrenderConfirmed)
	}
	finish()
}

// The wrapper records submitted identities but delegates every RPC to the real
// Temporal client. It never fabricates acceptance, results or stop observations.
type observedChildTemporal struct {
	client.Client
	mu    sync.Mutex
	calls map[string]engine.ChildDispatchInput
}

func (c *observedChildTemporal) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = make(map[string]engine.ChildDispatchInput)
	}
	c.calls[options.ID] = args[0].(engine.ChildDispatchInput)
	c.mu.Unlock()
	return c.Client.ExecuteWorkflow(ctx, options, workflow, args...)
}
func (c *observedChildTemporal) snapshot() map[string]engine.ChildDispatchInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]engine.ChildDispatchInput, len(c.calls))
	for id, in := range c.calls {
		out[id] = in
	}
	return out
}

func childQualificationKubernetes(t *testing.T) (*kubernetes.Clientset, string) {
	t.Helper()
	path := os.Getenv("GOOBERS_CHILD_QUALIFICATION_KUBECONFIG")
	if path == "" {
		t.Fatal("explicit private qualification kubeconfig required")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw.CurrentContext, "kind-haw-child-") {
		t.Fatal("only disposable haw-child kind contexts are allowed")
	}
	cfg, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(cfg.Host)
	if err != nil || endpoint.Hostname() != "127.0.0.1" {
		t.Fatal("qualification Kubernetes API must be literal loopback")
	}
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := api.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "haw-child-qualification-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := api.CoreV1().Namespaces().Delete(ctx, namespace.Name, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
	})
	_, err = api.CoreV1().ServiceAccounts(namespace.Name).Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "qualification-stage"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pods, err := api.CoreV1().Pods(namespace.Name).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, pod := range pods.Items {
					t.Logf("pod %s phase %s containers %+v", pod.Name, pod.Status.Phase, pod.Status.ContainerStatuses)
					data, err := api.CoreV1().Pods(namespace.Name).GetLogs(pod.Name, &corev1.PodLogOptions{LimitBytes: k8sptr.To[int64](8192)}).DoRaw(ctx)
					t.Logf("pod log %s: %s (%v)", pod.Name, data, err)
				}
			}
		}
	})
	return api, namespace.Name
}

func drainRealQueuedChild(t *testing.T, f childKitFixture) {
	t.Helper()
	s := f.writer.service
	store, err := executionGenerationStore(s.layout)
	if err != nil {
		t.Fatal(err)
	}
	retainer := &configgeneration.Retainer{Store: store}
	t.Cleanup(func() { _ = retainer.Close() })
	registry := newDaemonRunnerRegistry()
	dispatch := newDaemonTriggerService()
	dispatch.AttachDispatchContext(t.Context())
	dispatch.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: f.child.Identity.Gaggle, Workflow: f.writer.identity.Child.ParentWorkflow, RepoRef: f.parent.applied.Gaggles[0].Spec.Project, Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 3}}}, s.log))
	triggers := &durableTriggerService{queue: s.childQueue, dispatch: dispatch, observeChild: acceptedChildObserver(s.layout)}
	build := func(layout instance.Layout, generation string, set *instance.ConfigSet, report *validate.Report) (*schedulerDefinitions, error) {
		return buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: layout, Config: s.config, Definitions: set, Validation: report, RunnerRegistry: registry, ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}, PinnedGeneration: generation})
	}
	var wg sync.WaitGroup
	setup := &schedulerSetup{RunnerRegistry: registry, ChildRuntime: childRuntimeBuilderFor(s.layout, retainer, s.config, build)}
	if err := s.installQueuedChildren(setup, triggers, &wg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wg.Wait)
	if err := triggers.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for range 2 {
		if err := triggers.Drain(t.Context()); err != nil {
			logQueuedFactoryJournal(t, s, f.child.RunID)
			t.Fatal(err)
		}
	}
	child, err := s.childQueue.GetChild(t.Context(), f.child.Identity)
	if err != nil || child.State != triggerqueue.ChildCompleted || child.ResultRef == "" {
		logQueuedFactoryJournal(t, s, f.child.RunID)
		t.Fatal("real child did not complete", child.State, child.ResultRef, err)
	}
}
