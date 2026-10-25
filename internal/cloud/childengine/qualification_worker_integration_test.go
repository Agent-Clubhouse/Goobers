//go:build cloudintegration

package childengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childadmissionclient"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/podauth"
)

type qualificationWorkerConfig struct {
	GaggleNamespaces                                            map[string]string
	Address, Queue, Endpoint, KeyFile, InstanceID, SurrenderDir string
	DisablePodCustody                                           bool
	Script                                                      qualificationStageScript
}

// Phases: parent-start, parent-granted, parent-submitted, parent-resumed, child.
// A nil Result and empty Error continue the normal scripted happy path.
type qualificationStageScript map[string]qualificationStageStep
type qualificationStageStep struct {
	Barrier       string
	Result        *apiv1.ResultEnvelope
	Error         string
	HeartbeatLoss bool
}

func (s qualificationStages) step(ctx context.Context, phase string) (*engine.DispatchStageResult, error) {
	step := s.cfg.Script[phase]
	if state, ok := ctx.Value(qualificationHeartbeatKey{}).(*atomic.Bool); ok {
		state.Store(step.HeartbeatLoss)
	}
	if step.Barrier != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint+"/__qualification/stage-barrier/"+url.PathEscape(step.Barrier), nil)
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			return nil, fmt.Errorf("stage barrier HTTP %d", response.StatusCode)
		}
	}
	if step.Error != "" {
		return nil, errors.New(step.Error)
	}
	if step.Result != nil {
		return &engine.DispatchStageResult{ResultEnvelope: *step.Result}, nil
	}
	return nil, nil
}

// TestChildQualificationWorkerProcess is the subprocess entry point. In the
// normal suite it passes without starting anything; only StartWorker sets the
// private configuration path. The worker has HTTP and Temporal clients only.
func TestChildQualificationWorkerProcess(t *testing.T) {
	file := os.Getenv("GOOBERS_CHILD_QUALIFICATION_WORKER")
	if file == "" {
		return
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cfg qualificationWorkerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	key, err := podauth.LoadKeyRingFile(cfg.KeyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial(client.Options{HostPort: cfg.Address})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	registry := journal.NewRegistryScrubber()
	inner := childadmissionclient.AuthorityForWorker(cfg.Endpoint, cfg.Endpoint, "qualification-worker", key, registry)
	probeBlobs := &dispatcher.BlobClient{BaseURL: cfg.Endpoint, TokenSource: func() (string, error) { return key.MintWorkerBlob("qualification-worker", time.Minute) }}
	digest, canary := childresult.BlobPlaneCanary(cfg.InstanceID)
	authority := &childadmissionclient.ProbedAuthority{Inner: inner, Probe: func(ctx context.Context) error {
		got, err := probeBlobs.Get(ctx, digest)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, canary) {
			return errors.New("canary differs")
		}
		return nil
	}, Report: func(err error) { t.Logf("child authority withheld: %v", err) }}
	surrenders, err := dispatcher.NewSurrenderDir(cfg.SurrenderDir)
	if err != nil {
		t.Fatal(err)
	}
	stages := qualificationStages{cfg: cfg, key: key, surrenders: surrenders}
	w := worker.New(c, cfg.Queue, worker.Options{Identity: "qualification-worker", WorkerStopTimeout: time.Second, MaxHeartbeatThrottleInterval: engine.DispatchHeartbeatThrottleInterval, Interceptors: []interceptor.WorkerInterceptor{&qualificationDispatchFaults{initialLoss: cfg.Script["parent-start"].HeartbeatLoss}}})
	engine.RegisterWith(w, &engine.Activities{EngineChildren: childadmissionclient.ForWorker(cfg.Endpoint, "qualification-worker", key),
		ChildAuthority: authority, Dispatcher: stages, Surrenders: surrenders,
		Journal: &livejournal.HTTPEmitter{ChildOriginWorkerID: "qualification-worker", BaseURL: cfg.Endpoint, Minter: key}})
	// The parent uses the registered production DispatchStage activity. Only
	// its container executor and surrender storage are scripted. Generated
	// deterministic child execution retains its existing fixture boundary.
	for _, name := range []string{engine.ActRunDeterministic, "RunDeterministicCancelSafe"} {
		w.RegisterActivityWithOptions(stages.child, activity.RegisterOptions{Name: name, DisableAlreadyRegisteredCheck: true})
	}
	if err := w.Run(worker.InterruptCh()); err != nil {
		t.Fatal(err)
	}
}

type qualificationStages struct {
	cfg        qualificationWorkerConfig
	key        *podauth.SignedKey
	surrenders *dispatcher.SurrenderDir
}

// Dispatch uses the real pod dispatcher against fixture-owned execution.
// It never creates a handoff; the production observer derives that from custody.
func (s qualificationStages) Dispatch(ctx context.Context, attempt dispatcher.Attempt, eligible []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	d, err := s.dispatcher(attempt)
	if err != nil {
		return dispatcher.Report{}, err
	}
	if s.cfg.DisablePodCustody {
		// Explicit fault injection: remove the engine-installed observer, leaving
		// the production heartbeat and missing-custody refusal path unchanged.
		ctx = dispatcher.WithPodCustodyObserver(ctx, func(dispatcher.PodCustody) {})
	}
	return d.Dispatch(ctx, attempt, eligible)
}

func (s qualificationStages) AdoptPod(ctx context.Context, attempt dispatcher.Attempt, custody dispatcher.PodCustody) (dispatcher.Report, error) {
	d, err := s.dispatcher(attempt)
	if err != nil {
		return dispatcher.Report{}, err
	}
	return d.AdoptPod(ctx, attempt, custody)
}

func (s qualificationStages) dispatcher(attempt dispatcher.Attempt) (*dispatcher.Dispatcher, error) {
	pods := &qualificationPodClient{endpoint: s.cfg.Endpoint, attempt: attempt, script: s.cfg.Script}
	return dispatcher.New(dispatcher.Config{
		GaggleNamespaces: s.cfg.GaggleNamespaces,
		InstanceID:       s.cfg.InstanceID, Owner: "qualification-worker", EmbeddedVersion: "qualification",
		TokenMinter: s.key, KitWriter: qualificationKitWriter{},
		BlobEndpoint: s.cfg.Endpoint, WriteAPIBase: s.cfg.Endpoint,
		SupervisionInterval: 20 * time.Millisecond,
	}, pods, nil, dispatcher.PlaneSurrenderGate{Plane: s.surrenders}, nil)
}

// The simulated container does not consume a kit. Only this executor input is
// synthetic; pod rendering, custody publication, adoption and stop checks are real.
type qualificationKitWriter struct{}

func (qualificationKitWriter) WriteKit(context.Context, dispatcher.Attempt) (string, error) {
	return "sha256:" + strings.Repeat("a", 64), nil
}

func (s qualificationStages) parent(ctx context.Context, attempt dispatcher.Attempt) (engine.DispatchStageResult, error) {
	if attempt.Envelope == nil {
		return engine.DispatchStageResult{}, errors.New("scripted parent requires the production dispatch envelope")
	}
	env := *attempt.Envelope
	if result, err := s.step(ctx, "parent-start"); result != nil || err != nil {
		if result != nil {
			return *result, err
		}
		return engine.DispatchStageResult{}, err
	}
	for _, pointer := range env.ContextPointers {
		if !childresult.IsContinuationPointer(pointer) {
			continue
		}
		// Use a stage POD bearer, not the worker's credentials or control's Azure
		// handle. This is precisely the read a resumed parent needs to succeed.
		token, err := s.key.MintScoped(env.RunID, time.Minute, podauth.ScopeBlob)
		if err != nil {
			return engine.DispatchStageResult{}, err
		}
		blobs := &dispatcher.BlobClient{BaseURL: s.cfg.Endpoint, Token: token}
		receipt, err := blobs.Get(ctx, pointer.Artifact.Digest)
		if err != nil {
			return engine.DispatchStageResult{}, err
		}
		if journal.Digest(receipt) != pointer.Artifact.Digest || int64(len(receipt)) != pointer.Artifact.Size {
			return engine.DispatchStageResult{}, errors.New("child receipt bytes differ from continuation")
		}
		// Require the child's actual output, not just a successful blob GET.
		var sealed struct {
			Input childresult.TerminalResultInput `json:"input"`
			RunID string                          `json:"runId"`
		}
		if err := json.Unmarshal(receipt, &sealed); err != nil || sealed.Input.Summary != "qualification child result" || sealed.Input.State != "completed" || sealed.RunID == env.RunID || sealed.RunID == "" {
			return engine.DispatchStageResult{}, errors.New("child receipt lacks expected result")
		}
		if result, err := s.step(ctx, "parent-resumed"); result != nil || err != nil {
			if result != nil {
				return *result, err
			}
			return engine.DispatchStageResult{}, err
		}
		return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "parent read child receipt through blob plane"}}, nil
	}
	if env.ChildWorkflowOrigin == nil {
		return engine.DispatchStageResult{}, errors.New("parent has no committed child origin")
	}
	var access mcpio.ChildWorkflowAccess
	if err := json.Unmarshal([]byte(attempt.ChildWorkflowAccess), &access); err != nil {
		return engine.DispatchStageResult{}, err
	}
	if err := access.Validate(env.RunID); err != nil {
		return engine.DispatchStageResult{}, err
	}
	if result, err := s.step(ctx, "parent-granted"); result != nil || err != nil {
		if result != nil {
			return *result, err
		}
		return engine.DispatchStageResult{}, err
	}
	body, err := json.Marshal(map[string]string{"source": generatedChild})
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	path := strings.Replace(apicontract.ChildWorkflowStartPath, "{run}", env.RunID, 1)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+access.BearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(httpapi.HeaderIdempotencyKey, "qualification-child")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	if response.StatusCode != http.StatusAccepted {
		return engine.DispatchStageResult{}, fmt.Errorf("child start HTTP %d: %s", response.StatusCode, raw)
	}
	if result, err := s.step(ctx, "parent-submitted"); result != nil || err != nil {
		if result != nil {
			return *result, err
		}
		return engine.DispatchStageResult{}, err
	}
	// Acceptance is durable; wait for the production adapter to cancel the
	// parent execution. Returning a stage-authored ChildWait is impossible here.
	<-ctx.Done()
	return engine.DispatchStageResult{}, ctx.Err()
}

func (s qualificationStages) child(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun, _ string, _ string) (result engine.DispatchStageResult, err error) {
	ctx, stop := qualificationHeartbeat(ctx, s.cfg.Script["child"].HeartbeatLoss)
	defer func() {
		stop()
		if err != nil && errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), worker.ErrWorkerShutdown) {
			err = temporal.NewCanceledError()
		}
	}()
	if result, err := s.step(ctx, "child"); result != nil || err != nil {
		if result != nil {
			return *result, err
		}
		return engine.DispatchStageResult{}, err
	}
	// Terminal receipt capture reads the journaled output, as the local runner does.
	return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "qualification child result", Outputs: map[string]any{"summary": "qualification child result"}}}, nil
}

// Match engine.heartbeatDispatch: immediately, then every second, continuing
// through cancellation cleanup until the activity returns. The worker uses
// the production maximum RPC throttle so heartbeat replies deliver cancels.
type qualificationHeartbeatKey struct{}

func qualificationHeartbeat(ctx context.Context, loss bool) (context.Context, func()) {
	if !activity.IsActivity(ctx) {
		return ctx, func() {}
	}
	state := &atomic.Bool{}
	state.Store(loss)
	heartbeatCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var sequence uint64
	record := func() {
		if state.Load() {
			return
		}
		sequence++
		activity.RecordHeartbeat(heartbeatCtx, struct {
			Sequence    uint64    `json:"sequence"`
			GeneratedAt time.Time `json:"generatedAt"`
		}{sequence, time.Now().UTC()})
	}
	record()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				record()
			}
		}
	}()
	return context.WithValue(ctx, qualificationHeartbeatKey{}, state), func() { cancel(); <-done }
}

// Suppress only explicitly injected dispatch heartbeats, at the SDK boundary.
// Normal parent heartbeats come from production Activities.DispatchStage.
type qualificationDispatchFaults struct {
	interceptor.WorkerInterceptorBase
	initialLoss bool
}

func (f *qualificationDispatchFaults) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &qualificationDispatchInbound{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}, initialLoss: f.initialLoss}
}

type qualificationDispatchInbound struct {
	interceptor.ActivityInboundInterceptorBase
	initialLoss bool
	loss        atomic.Bool
}

func (i *qualificationDispatchInbound) Init(next interceptor.ActivityOutboundInterceptor) error {
	return i.Next.Init(&qualificationDispatchOutbound{ActivityOutboundInterceptorBase: interceptor.ActivityOutboundInterceptorBase{Next: next}, loss: &i.loss})
}
func (i *qualificationDispatchInbound) ExecuteActivity(ctx context.Context, input *interceptor.ExecuteActivityInput) (any, error) {
	if activity.GetInfo(ctx).ActivityType.Name == engine.ActDispatchStage {
		i.loss.Store(i.initialLoss)
		ctx = context.WithValue(ctx, qualificationHeartbeatKey{}, &i.loss)
	}
	return i.Next.ExecuteActivity(ctx, input)
}

type qualificationDispatchOutbound struct {
	interceptor.ActivityOutboundInterceptorBase
	loss *atomic.Bool
}

func (i *qualificationDispatchOutbound) RecordHeartbeat(ctx context.Context, details ...any) {
	if !i.loss.Load() {
		i.Next.RecordHeartbeat(ctx, details...)
	}
}
