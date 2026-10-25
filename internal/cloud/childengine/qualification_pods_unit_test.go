//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/podauth"
)

const qualificationPodNamespace = "qualification-example"

func TestQualificationPodAdoptionJoinsExistingExecutor(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	surrender, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	finished := make(chan struct{})
	pods := &qualificationPods{ctx: ctx, pods: make(map[string]*qualificationPod), stages: qualificationStages{surrenders: surrender}, execute: func(ctx context.Context, _ qualificationStages, _ dispatcher.Attempt) (engine.DispatchStageResult, error) {
		executions.Add(1)
		select {
		case <-finished:
		case <-ctx.Done():
			return engine.DispatchStageResult{}, ctx.Err()
		}
		return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	}}
	server := httptest.NewServer(pods)
	defer server.Close()
	defer pods.close()
	key, err := podauth.NewSignedKey([]byte("qualification-key-material-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	stages := qualificationStages{key: key, cfg: qualificationWorkerConfig{GaggleNamespaces: map[string]string{"example": qualificationPodNamespace}, Endpoint: server.URL, InstanceID: testInstance}, surrenders: surrender}
	attempt := dispatcher.Attempt{RunID: "qualification-run", Gaggle: "example", Workflow: "parent", Stage: "plan", Number: 2, PodAttempt: 7, OwningWorkflowID: "qualification-workflow", ChildWorkflowAccess: "scripted-access"}
	receipt := make(chan dispatcher.PodCustody, 1)
	dispatchCtx, stopWorker := context.WithCancel(ctx)
	dispatchCtx = dispatcher.WithPodCustodyObserver(dispatchCtx, func(c dispatcher.PodCustody) { receipt <- c })
	dispatchCtx = dispatcher.WithWorkerHandoffDecision(dispatchCtx, func() bool { return true })
	dispatched := make(chan error, 1)
	go func() {
		_, err := stages.Dispatch(dispatchCtx, attempt, []dispatcher.RunnerSpec{{Name: "qualification", HostKind: "image", Host: "qualification:qualification", OS: "linux"}})
		dispatched <- err
	}()
	var custody dispatcher.PodCustody
	select {
	case custody = <-receipt:
	case err := <-dispatched:
		t.Fatalf("dispatch before receipt: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if custody.Attempt != 7 || custody.Pod != dispatcher.PodName(attempt) || custody.Namespace != qualificationPodNamespace || custody.UID == "" || !custody.Custody {
		t.Fatalf("invalid receipt: %+v", custody)
	}
	stopWorker()
	select {
	case err := <-dispatched:
		if !errors.Is(err, dispatcher.ErrWorkerHandoff) {
			t.Fatalf("worker handoff: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A fresh dispatcher represents the replacement process. Adoption checks
	// every production identity field before supervising the existing object.
	replacement := qualificationStages{key: key, cfg: stages.cfg, surrenders: surrender}
	wrong := custody
	wrong.UID = "replaced"
	if _, err := replacement.AdoptPod(ctx, attempt, wrong); err == nil {
		t.Fatal("adopted replaced UID")
	}
	wrongAttempt := attempt
	wrongAttempt.PodAttempt++
	if _, err := replacement.AdoptPod(ctx, wrongAttempt, custody); err == nil {
		t.Fatal("adopted changed physical attempt")
	}
	close(finished)
	report, err := replacement.AdoptPod(ctx, attempt, custody)
	if err != nil {
		t.Fatal(err)
	}
	if !report.ParentWritersStopped || !report.SurrenderConfirmed || !report.Disposed || report.Pod != custody.Pod {
		t.Fatalf("adoption report: %+v", report)
	}
	if executions.Load() != 1 || pods.created != 1 {
		t.Fatalf("reran existing executor: executions=%d creates=%d", executions.Load(), pods.created)
	}
	if _, err := dispatcher.ReadSurrenderedResult(ctx, surrender, attempt.RunID, attempt.Stage, 7); err != nil {
		t.Fatalf("replacement lost physical-attempt surrender: %v", err)
	}
	if _, err := replacement.AdoptPod(ctx, attempt, custody); err == nil {
		t.Fatal("adopted missing pod")
	}
}

func TestQualificationPodCustodyOptOutKeepsRealDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	surrender, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pods := &qualificationPods{ctx: ctx, pods: make(map[string]*qualificationPod), stages: qualificationStages{surrenders: surrender}, execute: func(context.Context, qualificationStages, dispatcher.Attempt) (engine.DispatchStageResult, error) {
		return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	}}
	server := httptest.NewServer(pods)
	defer server.Close()
	defer pods.close()
	key, err := podauth.NewSignedKey([]byte("qualification-key-material-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	stages := qualificationStages{key: key, cfg: qualificationWorkerConfig{GaggleNamespaces: map[string]string{"example": qualificationPodNamespace}, Endpoint: server.URL, InstanceID: testInstance, DisablePodCustody: true}, surrenders: surrender}
	var observed atomic.Bool
	ctx = dispatcher.WithPodCustodyObserver(ctx, func(dispatcher.PodCustody) { observed.Store(true) })
	report, err := stages.Dispatch(ctx, dispatcher.Attempt{RunID: "run", Gaggle: "example", Stage: "plan", Number: 1, OwningWorkflowID: "run", ChildWorkflowAccess: "scripted-access"}, []dispatcher.RunnerSpec{{Name: "qualification", HostKind: "image", Host: "qualification:qualification", OS: "linux"}})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Load() || pods.created != 1 || !report.ParentWritersStopped {
		t.Fatalf("opt-out bypassed dispatch or published custody: observed=%v creates=%d report=%+v", observed.Load(), pods.created, report)
	}
}

func TestQualificationPodStopWaitsForWriterJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	surrender, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := podauth.NewSignedKey([]byte("qualification-key-material-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	join := newQualificationBarrier()
	pods := &qualificationPods{ctx: ctx, pods: make(map[string]*qualificationPod), stages: qualificationStages{surrenders: surrender}, execute: func(ctx context.Context, _ qualificationStages, _ dispatcher.Attempt) (engine.DispatchStageResult, error) {
		<-ctx.Done()
		join.hold(context.WithoutCancel(ctx))
		return engine.DispatchStageResult{}, ctx.Err()
	}}
	server := httptest.NewServer(pods)
	defer server.Close()
	defer pods.close()
	defer join.Release()
	stages := qualificationStages{key: key, cfg: qualificationWorkerConfig{Endpoint: server.URL, InstanceID: testInstance, GaggleNamespaces: map[string]string{"example": qualificationPodNamespace}}, surrenders: surrender}
	receipt := make(chan dispatcher.PodCustody, 1)
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	ctx = dispatcher.WithPodCustodyObserver(ctx, func(c dispatcher.PodCustody) { receipt <- c })
	reports := make(chan dispatcher.Report, 1)
	go func() {
		report, _ := stages.Dispatch(ctx, dispatcher.Attempt{RunID: "run", Gaggle: "example", Stage: "plan", Number: 1, OwningWorkflowID: "run", ChildWorkflowAccess: "scripted-access"}, []dispatcher.RunnerSpec{{Name: "qualification", HostKind: "image", Host: "qualification:qualification", OS: "linux"}})
		reports <- report
	}()
	var custody dispatcher.PodCustody
	select {
	case custody = <-receipt:
	case report := <-reports:
		t.Fatalf("dispatch before receipt: %+v", report)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	cleanup, cleanupCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cleanupCancel()
	if err := join.Wait(cleanup); err != nil {
		t.Fatal(err)
	}
	api := &qualificationPodClient{endpoint: server.URL}
	pod, err := api.GetPod(cleanup, custody.Namespace, custody.Pod)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Status.ContainerStatuses[0].State.Terminated != nil || pod.DeletionTimestamp == nil {
		t.Fatal("reported termination before writer join or failed to request exact-UID stop")
	}
	if err := api.FinalizePodWithIdentity(cleanup, custody.Namespace, custody.Pod, pod.UID); err == nil {
		t.Fatal("finalized live writers")
	}
	select {
	case report := <-reports:
		t.Fatalf("dispatch returned before join: %+v", report)
	default:
	}
	join.Release()
	select {
	case report := <-reports:
		if !report.ParentWritersStopped {
			t.Fatalf("joined writers lack stop proof: %+v", report)
		}
	case <-cleanup.Done():
		t.Fatal(cleanup.Err())
	}
}
