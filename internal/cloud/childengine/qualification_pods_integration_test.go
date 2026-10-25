//go:build cloudintegration

package childengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/podauth"
)

const qualificationPodRoute = "/__qualification/pods"

// Pod objects, executors and surrender files belong to the fixture, never the
// worker. A replacement worker can only GET/adopt the retained UID; Create of
// the same name fails, and adoption does not rerun the stage or submission.
type qualificationPods struct {
	ctx     context.Context
	stages  qualificationStages
	mu      sync.Mutex
	pods    map[string]*qualificationPod
	created int
	logf    func(string, ...any)
	execute func(context.Context, qualificationStages, dispatcher.Attempt) (engine.DispatchStageResult, error)
}
type qualificationPod struct {
	pod    *corev1.Pod
	cancel context.CancelFunc
	done   chan struct{}
	loss   atomic.Bool
}
type qualificationPodRequest struct {
	Operation, Namespace, Name string
	UID                        types.UID
	Pod                        *corev1.Pod
	Attempt                    dispatcher.Attempt
	Script                     qualificationStageScript
	Selector                   map[string]string
}
type qualificationPodResponse struct {
	Pod           *corev1.Pod
	Pods          []corev1.Pod
	HeartbeatLoss bool
	Error         string
	NotFound      bool
}

func (h *qualificationHarness) initPods() {
	h.surrenderDir = h.t.TempDir()
	plane, err := dispatcher.NewSurrenderDir(h.surrenderDir)
	if err != nil {
		h.t.Fatal(err)
	}
	keys, err := podauth.LoadKeyRingFile(h.keyFile, "")
	if err != nil {
		h.t.Fatal(err)
	}
	h.pods = &qualificationPods{ctx: h.ctx, logf: h.t.Logf, pods: make(map[string]*qualificationPod), stages: qualificationStages{
		cfg: qualificationWorkerConfig{Endpoint: h.server.URL, InstanceID: testInstance},
		key: keys, surrenders: plane,
	}}
	h.t.Cleanup(h.pods.close)
}

func (p *qualificationPods) close() {
	p.mu.Lock()
	all := make([]*qualificationPod, 0, len(p.pods))
	for _, pod := range p.pods {
		pod.cancel()
		all = append(all, pod)
	}
	p.mu.Unlock()
	for _, pod := range all {
		<-pod.done
	}
}

func (p *qualificationPods) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in qualificationPodRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := p.call(r.Context(), in)
	if err != nil {
		out.Error = err.Error()
		out.NotFound = apierrors.IsNotFound(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (p *qualificationPods) call(ctx context.Context, in qualificationPodRequest) (qualificationPodResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := qualificationPodResponse{}
	key := in.Namespace + "/" + in.Name
	if in.Operation == "create" {
		if in.Pod == nil {
			return out, errors.New("missing pod")
		}
		key = in.Pod.Namespace + "/" + in.Pod.Name
		if p.pods[key] != nil {
			return out, errors.New("pod already exists; replacement forbidden")
		}
		pod := in.Pod.DeepCopy()
		pod.UID = types.UID(uuid.NewString())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: dispatcher.StageContainerName, ContainerID: "qualification://" + string(pod.UID), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
		execution, cancel := context.WithCancel(p.ctx)
		held := &qualificationPod{pod: pod, cancel: cancel, done: make(chan struct{})}
		p.pods[key] = held
		p.created++
		stages := p.stages
		stages.cfg.Script = in.Script
		go p.run(context.WithValue(execution, qualificationHeartbeatKey{}, &held.loss), held, stages, in.Attempt)
		out.Pod = pod.DeepCopy()
		return out, nil
	}
	if in.Operation == "list" {
		for _, held := range p.pods {
			if held.pod.Namespace != in.Namespace {
				continue
			}
			match := true
			for k, v := range in.Selector {
				if held.pod.Labels[k] != v {
					match = false
					break
				}
			}
			if match {
				out.Pods = append(out.Pods, *held.pod.DeepCopy())
			}
		}
		return out, nil
	}
	held := p.pods[key]
	if held == nil {
		return out, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, in.Name)
	}
	if in.UID != "" && in.UID != held.pod.UID {
		return out, errors.New("pod UID precondition failed")
	}
	switch in.Operation {
	case "get":
		out.Pod = held.pod.DeepCopy()
		out.HeartbeatLoss = held.loss.Load()
	case "delete":
		held.cancel()
		// Retain the finalizer and Running status until the executor has joined.
		stamp := metav1.Now()
		held.pod.DeletionTimestamp = &stamp
		if len(held.pod.Finalizers) == 0 {
			p.mu.Unlock()
			select {
			case <-held.done:
			case <-ctx.Done():
				p.mu.Lock()
				return out, ctx.Err()
			}
			p.mu.Lock()
			delete(p.pods, key)
		}
	case "finalize":
		select {
		case <-held.done:
		default:
			return out, errors.New("writers still running")
		}
		if held.pod.DeletionTimestamp == nil {
			return out, errors.New("pod not deleting")
		}
		delete(p.pods, key)
	default:
		return out, errors.New("unknown pod operation")
	}
	return out, nil
}

func (p *qualificationPods) run(ctx context.Context, held *qualificationPod, stages qualificationStages, attempt dispatcher.Attempt) {
	execute := p.execute
	if execute == nil {
		execute = func(ctx context.Context, s qualificationStages, a dispatcher.Attempt) (engine.DispatchStageResult, error) {
			return s.parent(ctx, a)
		}
	}
	result, err := execute(ctx, stages, attempt)
	if result.Status != "" {
		data, marshalErr := json.Marshal(dispatcher.SurrenderedResult{ContractVersion: dispatcher.CurrentSurrenderContract, Result: result.ResultEnvelope, ObservedUsageReported: true, RecoveryAcknowledged: true})
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		if marshalErr == nil {
			marshalErr = stages.surrenders.Put(cleanup, attempt.RunID, attempt.Stage, attempt.IdentityAttempt(), data)
		}
		cancel()
		if marshalErr != nil {
			err = errors.Join(err, marshalErr)
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) && p.logf != nil {
		p.logf("qualification pod executor: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	phase, code := corev1.PodSucceeded, int32(0)
	if err != nil {
		phase = corev1.PodFailed
		code = 1
	}
	held.pod.Status.Phase = phase
	held.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, FinishedAt: metav1.Now()}}
	close(held.done)
}

// Each real dispatcher gets its own RPC client/attempt. No server operation
// trusts this transport as production authority: this is a fixture PodAPI only.
type qualificationPodClient struct {
	endpoint string
	attempt  dispatcher.Attempt
	script   qualificationStageScript
}

func (p *qualificationPodClient) call(ctx context.Context, in qualificationPodRequest) (qualificationPodResponse, error) {
	var out qualificationPodResponse
	data, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+qualificationPodRoute, bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return out, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return out, fmt.Errorf("pod fixture HTTP %d", response.StatusCode)
	}
	if err = json.NewDecoder(response.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.NotFound {
		return out, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, in.Name)
	}
	if out.Error != "" {
		return out, errors.New(out.Error)
	}
	if in.Operation == "get" {
		if loss, ok := ctx.Value(qualificationHeartbeatKey{}).(*atomic.Bool); ok {
			loss.Store(out.HeartbeatLoss)
		}
	}
	return out, nil
}
func (p *qualificationPodClient) CreatePodWithIdentity(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	out, err := p.call(ctx, qualificationPodRequest{Operation: "create", Pod: pod, Attempt: p.attempt, Script: p.script})
	return out.Pod, err
}
func (p *qualificationPodClient) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	_, err := p.CreatePodWithIdentity(ctx, pod)
	return err
}
func (p *qualificationPodClient) GetPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	out, err := p.call(ctx, qualificationPodRequest{Operation: "get", Namespace: ns, Name: name})
	return out.Pod, err
}
func (p *qualificationPodClient) DeletePod(ctx context.Context, ns, name string) error {
	return p.DeletePodWithIdentity(ctx, ns, name, "")
}
func (p *qualificationPodClient) DeletePodWithIdentity(ctx context.Context, ns, name string, uid types.UID) error {
	_, err := p.call(ctx, qualificationPodRequest{Operation: "delete", Namespace: ns, Name: name, UID: uid})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
func (p *qualificationPodClient) FinalizePodWithIdentity(ctx context.Context, ns, name string, uid types.UID) error {
	_, err := p.call(ctx, qualificationPodRequest{Operation: "finalize", Namespace: ns, Name: name, UID: uid})
	return err
}
func (p *qualificationPodClient) ListPods(ctx context.Context, ns string, selector map[string]string) ([]corev1.Pod, error) {
	out, err := p.call(ctx, qualificationPodRequest{Operation: "list", Namespace: ns, Selector: selector})
	return out.Pods, err
}
func (p *qualificationPodClient) GetDeployment(context.Context, string, string) (*appsv1.Deployment, error) {
	return nil, errors.New("qualification supports image runners only")
}
func (p *qualificationPodClient) GetPersistentVolumeClaim(_ context.Context, _ string, name string) (*corev1.PersistentVolumeClaim, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, name)
}

// SetPodCustodyReporting is an explicit missing-custody fault. Default is ON.
func (h *qualificationHarness) SetPodCustodyReporting(enabled bool) {
	if h.worker != nil {
		h.t.Fatal("set pod custody reporting before starting the worker")
	}
	h.disablePodCustody = !enabled
}

// WaitForPodCustody waits for a receipt persisted by Temporal, not merely the
// local observer. The first heartbeat (before Create) has no custody. Call this
// after holding a stage barrier and before KillWorker in adoption scenarios.
func (h *qualificationHarness) WaitForPodCustody(run client.WorkflowRun) dispatcher.PodCustody {
	h.t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		description, err := h.temporal.Client().DescribeWorkflowExecution(h.ctx, run.GetID(), run.GetRunID())
		if err != nil {
			h.t.Fatal(err)
		}
		for _, pending := range description.PendingActivities {
			if pending.ActivityType.GetName() != engine.ActDispatchStage || pending.HeartbeatDetails == nil {
				continue
			}
			var custody dispatcher.PodCustody
			if err := converter.GetDefaultDataConverter().FromPayloads(pending.HeartbeatDetails, &custody); err != nil {
				h.t.Fatal(err)
			}
			if custody.Custody && custody.UID != "" {
				pod, err := (&qualificationPodClient{endpoint: h.server.URL}).GetPod(h.ctx, custody.Namespace, custody.Pod)
				if err != nil || pod == nil || string(pod.UID) != custody.UID {
					h.t.Fatalf("persisted custody does not match live pod: %v", err)
				}
				return custody
			}
		}
		select {
		case <-ticker.C:
		case <-h.ctx.Done():
			h.t.Fatal("waiting for Temporal pod custody:", h.ctx.Err())
		}
	}
}
