//go:build cloudintegration

package childengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginechildplane"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/providers"
)

// qualificationHarness runs the production control HTTP planes and engine.Run
// against PostgreSQL, Azurite and Temporal. Production DispatchStage adapts
// accepted custody; only its container executor is scripted in a separate
// worker process with no database or storage credentials.
// See qualification.md before adding scenarios or fault injection.
type qualificationHarness struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	// owner is the migration-owner pool, for setup that only migrations perform
	// in production. Production paths under test always use pool.
	owner                                 *pgxpool.Pool
	blobs                                 *objstore.Azure
	objects                               *objstore.Metadata
	temporal                              *testsuite.DevServer
	applied                               qualificationConfig
	configDir, configFile, keyFile, queue string
	stageScript                           qualificationStageScript
	hooks                                 qualificationHooks
	input                                 engine.RunInput
	server                                *httptest.Server
	routerMu                              sync.RWMutex
	handler                               http.Handler
	epoch                                 *qualificationEpoch
	worker                                *exec.Cmd
	workerDone                            chan error
	workerLogs                            []*bytes.Buffer
	slotLimit                             int
}

type qualificationEpoch struct {
	manager   *lease.Manager
	handle    *lease.Handle
	store     *acceptance.Store
	backend   *journalpg.Backend
	writer    *livejournal.Writer
	scheduler *localscheduler.Scheduler
	log       *journalpg.SchedulerLog
	cancel    context.CancelFunc
	done      chan struct{}
	release   func()
}

type qualificationConfig struct {
	mu        sync.RWMutex
	candidate cloudconfig.Candidate
}

func (c *qualificationConfig) Applied() (cloudconfig.Candidate, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.candidate, c.candidate.Config != nil
}
func (c *qualificationConfig) Admit(digest, gaggle, workflow string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if digest != c.candidate.Digest || gaggle != "example" || workflow != "default-implement" {
		return errors.New("qualification generation mismatch")
	}
	return nil
}

func newQualificationHarness(t *testing.T) *qualificationHarness {
	t.Helper()
	for _, name := range []string{"GOOBERS_TEST_POSTGRES_URL", "GOOBERS_TEST_AZURITE_ACCOUNT", "GOOBERS_TEST_AZURITE_KEY", "GOOBERS_TEST_AZURITE_URL"} {
		if os.Getenv(name) == "" {
			if os.Getenv("TESTDEP_STRICT") == "1" {
				t.Fatalf("qualification requires %s", name)
			}
			t.Skipf("qualification requires %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	f := newFamily(t)
	if err := f.manager.Release(ctx, f.handle); err != nil {
		t.Fatal(err)
	}
	h := &qualificationHarness{t: t, ctx: ctx, pool: controllerPool(t, f.pool), owner: f.pool, queue: "child-qualification-" + uuid.NewString(), slotLimit: 1}
	// Register first so all worker processes are joined before their output is
	// read, including workers replaced during a fault scenario.
	t.Cleanup(func() {
		if t.Failed() {
			for i, output := range h.workerLogs {
				t.Logf("qualification worker process %d output:\n%s", i+1, output.String())
			}
		}
	})
	h.blobs = qualificationAzure(t)
	var err error
	h.objects, err = objstore.NewMetadataWithOptions(h.pool, h.blobs, objstore.MetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h.temporal, err = temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.temporal.Stop(); err != nil {
			t.Error(err)
		}
	})
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	layout := instance.NewLayout(root)
	h.configDir = layout.ConfigDir()
	h.configFile = layout.ConfigFile()
	if err := os.WriteFile(filepath.Join(h.configDir, "gaggles", "example", "workflows", "default-implement.yaml"), []byte(childEnabledParent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	// The production config-load refusal remains intact. Only this instance's
	// in-memory configuration and started RunInput opt into qualification.
	enabled := true
	cfg.Engine = &instance.EngineConfig{ChildWorkflows: &instance.EngineChildWorkflowsConfig{Enabled: enabled}}
	set := loadSet(t, h.configDir)
	selected := childworkflow.ParentSelection{Gaggle: "example", Workflow: "default-implement", Stage: "plan"}
	_, machine, err := childworkflow.StageCatalog(cfg, set, selected, childworkflow.BackendEngine, childworkflow.OfflineGooberAdmission)
	if err != nil {
		t.Fatal(err)
	}
	workflowDigest, gooberDigest, _ := parentPins(t, cfg, h.configDir, set)
	archive, digest, err := configgeneration.CaptureForInstance(ctx, h.configDir, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.objects.RecordGeneration(ctx, testInstance, digest, archive); err != nil {
		t.Fatal(err)
	}
	h.applied.candidate = cloudconfig.Candidate{Digest: digest, Config: cfg, Set: set}
	h.input = engine.RunInput{InstanceID: testInstance, Gaggle: "example", WorkflowName: machine.Def.Name, Version: machine.Def.Version,
		DSLVersion: machine.Def.DSLVersion, Annotations: machine.Def.Annotations, Spec: machine.Def.Spec, WorkflowDigest: workflowDigest, GooberDigest: gooberDigest, ConfigGeneration: digest,
		RepoRef: gaggleProject(t, set, "example"), BranchNamespace: providers.NormalizeBranchNamespace(""), LiveJournal: true, PreviewFeaturesEnabled: &enabled, ChildWorkflowsEnabled: true, Placements: []engine.PinnedPlacement{{Stage: "plan", Queue: h.queue, Eligible: []dispatcher.RunnerSpec{{Name: "qualification", HostKind: "image", OS: "linux"}}}}}
	h.keyFile = filepath.Join(t.TempDir(), "keyring.json")
	raw, err := json.Marshal(map[string]podauth.SigningKey{"current": {ID: "qualification", Key: []byte("qualification-key-material-32bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.keyFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewUnstartedServer(http.HandlerFunc(h.serveHTTP))
	h.server.Config.ErrorLog = log.New(qualificationLogWriter{t}, "qualification HTTP: ", 0)
	h.server.Start()
	t.Cleanup(h.server.Close)
	h.startControl()
	t.Cleanup(h.stopControl)
	canary, data := childresult.BlobPlaneCanary(testInstance)
	if err := h.blobs.Put(ctx, canary, data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.KillWorker)
	return h
}

func qualificationAzure(t *testing.T) *objstore.Azure {
	t.Helper()
	endpoint, account, key := os.Getenv("GOOBERS_TEST_AZURITE_URL"), os.Getenv("GOOBERS_TEST_AZURITE_ACCOUNT"), os.Getenv("GOOBERS_TEST_AZURITE_KEY")
	credential, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := azblob.NewClientWithSharedKeyCredential(endpoint, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	container := "child-qualification-" + uuid.NewString()
	if _, err := c.CreateContainer(t.Context(), container, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := c.DeleteContainer(ctx, container, nil); err != nil {
			t.Error(err)
		}
	})
	store, err := objstore.NewAzureSharedKey(endpoint, container, account, key)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (h *qualificationHarness) startControl() {
	t := h.t
	manager, err := lease.New(lease.Config{Pool: h.pool, InstanceID: testInstance, Name: lease.ControllerName, Holder: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	e := &qualificationEpoch{manager: manager, handle: handle, cancel: cancel, done: make(chan struct{})}
	h.epoch = e
	go func() {
		defer close(e.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := manager.Renew(ctx, handle); err != nil && ctx.Err() == nil {
					t.Errorf("renew qualification lease: %v", err)
					return
				}
			}
		}
	}()
	e.store, err = acceptance.New(h.pool, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	e.store.BindFence(manager, handle)
	e.store.BindChildBlobs(h.blobs)
	e.backend, err = journalpg.New(journalpg.Config{Pool: h.pool, InstanceID: testInstance, Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	e.writer, err = livejournal.NewWriter(func(string) (string, bool) { return qualificationCloudJournalRunsRoot, true }, livejournal.WithBackend(e.backend))
	if err != nil {
		t.Fatal(err)
	}
	e.log, err = e.backend.OpenSchedulerLog(ctx, func(err error) { t.Errorf("scheduler journal: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	// Mirror controlscheduler_wire.go: the epoch's PG journal log and its
	// lease-bound claims store are separate scheduler dependencies. A PG log
	// has no directory, so leaving the default file state breaks Reload.
	// Use the same clock-skew wrapper as production Prepare as well.
	state, err := cloudclaims.New(h.pool, testInstance, manager, handle)
	if err != nil {
		t.Fatal(err)
	}
	e.scheduler = localscheduler.New(h.entries(), e.log,
		localscheduler.WithSchedulerState(localscheduler.SkewTolerantState{SchedulerState: state}),
		localscheduler.WithRunDirReconciler(schedstart.NewReconciler(h.openRuns)))
	if err := e.scheduler.Reconcile("", time.Now()); err != nil {
		t.Fatal(err)
	}
	fenced := Fenced{Pool: h.pool, InstanceID: testInstance, Lease: manager, Owner: func() *lease.Handle { return handle }, Blobs: h.blobs, Objects: h.objects,
		Applied: &h.applied, Wait: time.Minute, Actor: "qualification-control", Now: time.Now, Adoption: &EpochAdoption{}}
	revocations := &cloudauth.Revocations{Pool: h.pool, InstanceID: testInstance}
	auth, err := cloudauth.New(cloudauth.Config{Mode: "signed", KeyFile: h.keyFile, Revocations: revocations, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context, id string) (journal.RunReader, error) {
		return e.backend.OpenRunReader(ctx, journal.RunKey{Gaggle: "example", RunID: id})
	}
	authority := FencedAuthority{Fenced: fenced, OriginTemporal: h.temporal.Client(), OriginJournal: e.writer, Journal: open, Execution: func(ctx context.Context, id string) (startfence.StartedExecution, error) {
		return startfence.DescribeStarted(ctx, h.temporal.Client(), id)
	}, Grants: auth.GrantKeys}
	protocol := FencedProtocol{Fenced: fenced, Journal: open, Capacity: &SchedulerCapacity{Slots: e.scheduler}}
	admissionsPlane, err := enginechildplane.New(fenced)
	if err != nil {
		t.Fatal(err)
	}
	authorityPlane, err := enginechildplane.NewAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	protocolPlane, err := enginechildplane.NewProtocol(protocol)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(struct{ readservice.Reader }{}, httpapi.RequireRoles(), log.New(qualificationLogWriter{t}, "qualification control: ", 0),
		httpapi.WithAuthenticator(auth), httpapi.WithJournalService(e.writer), httpapi.WithBlobService(h.blobs), httpapi.WithLeaseFreeBlobPlane(),
		httpapi.WithEngineChildAdmissions(admissionsPlane), httpapi.WithEngineChildAuthority(authorityPlane), httpapi.WithEngineChildProtocol(protocolPlane),
		httpapi.WithChildWorkflowService(FencedChildWorkflows{Authority: authority}))
	if err != nil {
		t.Fatal(err)
	}
	h.routerMu.Lock()
	h.handler = handler
	h.routerMu.Unlock()
}

func (h *qualificationHarness) entries() []localscheduler.WorkflowEntry {
	return []localscheduler.WorkflowEntry{{Gaggle: "example", Workflow: "default-implement", RepoRef: h.input.RepoRef,
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: int32(h.slotLimit), MaxRunsPerHour: 1000}}}
}
func (h *qualificationHarness) stopControl() {
	h.routerMu.Lock()
	h.handler = nil
	h.routerMu.Unlock()
	if h.epoch == nil {
		return
	}
	e := h.epoch
	h.epoch = nil
	e.cancel()
	<-e.done
	if e.release != nil {
		e.release()
	}
	if e.writer != nil {
		e.writer.Close()
	}
	if e.log != nil {
		_ = e.log.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.manager.Release(ctx, e.handle); err != nil {
		h.t.Error(err)
	}
}

// StartParent uses the real acceptance/start fence and pins the opt-in input.
// Start the worker only after confirmation so it cannot outrun its start pins.
func (h *qualificationHarness) StartParent() client.WorkflowRun {
	h.KillWorker()
	record, _, err := h.epoch.store.Accept(h.ctx, "qualification-parent", "qualification", []byte(`{"workflow":"default-implement"}`), time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	in := h.input
	in.AcceptanceID = record.ID
	starter := startfence.New(h.temporal.Client(), h.queue, acceptance.NewStartAdmission(h.epoch.store), &h.applied)
	result, err := starter.Start(h.ctx, in)
	if err != nil {
		h.t.Fatal(err)
	}
	h.input.RunID = result.RunID
	release, ok, reason := h.epoch.scheduler.ReserveContinuation(result.RunID, "example", "default-implement")
	if !ok {
		h.t.Fatal(reason)
	}
	h.epoch.release = release
	h.StartWorker()
	return h.temporal.Client().GetWorkflow(h.ctx, result.RunID, "")
}

// KillWorker kills only this harness's child PID and joins it. Restart with
// StartWorker; all workflow/custody/journal state remains on the real services.
func (h *qualificationHarness) KillWorker() {
	if h.worker == nil {
		return
	}
	_ = h.worker.Process.Kill()
	<-h.workerDone
	h.worker = nil
}
func (h *qualificationHarness) StartWorker() {
	if h.worker != nil {
		h.t.Fatal("worker already running")
	}
	cfg := qualificationWorkerConfig{Address: h.temporal.FrontendHostPort(), Queue: h.queue, Endpoint: h.server.URL, KeyFile: h.keyFile, InstanceID: testInstance, Script: h.stageScript}
	file := filepath.Join(h.t.TempDir(), "worker.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	output := new(bytes.Buffer)
	h.workerLogs = append(h.workerLogs, output)
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildQualificationWorkerProcess$", "-test.v")
	// Pass only test transport configuration: no database or Azurite credentials.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GOOBERS_CHILD_QUALIFICATION_WORKER=" + file}
	// The same comparable writer makes os/exec serialize stdout/stderr writes;
	// cleanup reads the buffer only after Wait has joined the copy goroutine.
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.worker = cmd
	h.workerDone = make(chan error, 1)
	go func() { h.workerDone <- cmd.Wait() }()
}

// SetSlotLimit changes admission capacity without changing running inputs.
func (h *qualificationHarness) SetSlotLimit(limit int) {
	if limit < 1 {
		h.t.Fatal("slot limit must be positive")
	}
	h.slotLimit = limit
	if err := h.epoch.scheduler.Reload(h.entries(), nil, time.Now(), "", "replacement"); err != nil {
		h.t.Fatal(err)
	}
}

// ResetParent issues a real Temporal Reset RPC at the scenario's chosen
// WorkflowTaskCompleted event. The scenario decides which refusal to assert.
func (h *qualificationHarness) ResetParent(run client.WorkflowRun, eventID int64) string {
	response, err := h.temporal.Client().WorkflowService().ResetWorkflowExecution(h.ctx, &workflowservice.ResetWorkflowExecutionRequest{
		Namespace: "default", WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: run.GetID(), RunId: run.GetRunID()},
		Reason: "child qualification", WorkflowTaskFinishEventId: eventID, RequestId: uuid.NewString()})
	if err != nil {
		h.t.Fatal(err)
	}
	return response.RunId
}

func TestChildQualificationHappyPath(t *testing.T) {
	h := newQualificationHarness(t)
	run := h.StartParent()
	var result engine.RunResult
	if err := run.Get(h.ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "parent read child receipt through blob plane" {
		t.Fatalf("parent result: %+v", result)
	}
	var count int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND state='completed'`, testInstance).Scan(&count); err != nil || count != 1 {
		t.Fatalf("completed children=%d: %v", count, err)
	}
	h.assertProductionChildHandoff(run)
	t.Log("qualification: production DispatchStage observed acceptance and stopped its scripted executor; real PG + Azurite + Temporal parent/child completed; parent consumed the sealed receipt through the authenticated pod blob plane")
}

// FailoverControl retires the old HTTP/journal writers, releases the real
// lease, and composes a successor with freshly reconstructed scheduler slots.
func (h *qualificationHarness) FailoverControl() { h.stopControl(); h.startControl() }

func (h *qualificationHarness) openRuns(ctx context.Context) ([]schedstart.OpenRun, error) {
	if h.input.RunID == "" {
		return nil, nil
	}
	started, err := startfence.DescribeStarted(ctx, h.temporal.Client(), h.input.RunID)
	if err != nil {
		return nil, err
	}
	if started.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return nil, nil
	}
	parked, err := h.epoch.backend.ParkedOnChild(ctx, []string{h.input.RunID})
	if err != nil {
		return nil, err
	}
	return []schedstart.OpenRun{{RunID: h.input.RunID, Identity: localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "default-implement"}, Suspended: parked[h.input.RunID]}}, nil
}

// ReloadConfig captures a new retained generation after edit changes the test
// config tree. Applied readers switch atomically; already-started inputs and
// retained generations stay unchanged. The test-only instance opt-in stays on.
func (h *qualificationHarness) ReloadConfig(edit func(string)) string {
	edit(h.configDir)
	current, _ := h.applied.Applied()
	return h.reloadConfig(current.Config.Engine.ChildWorkflows.Enabled)
}

func (h *qualificationHarness) reloadConfig(enabled bool) string {
	cfg := h.reloadInstanceConfig(enabled)
	set := loadSet(h.t, h.configDir)
	archive, digest, err := configgeneration.CaptureForInstance(h.ctx, h.configDir, testInstance)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.objects.RecordGeneration(h.ctx, testInstance, digest, archive); err != nil {
		h.t.Fatal(err)
	}
	h.applied.mu.Lock()
	h.applied.candidate = cloudconfig.Candidate{Digest: digest, Config: cfg, Set: set}
	h.applied.mu.Unlock()
	return digest
}

// ReloadInstanceConfig reloads instance.yaml through production validation and
// also captures the definition tree. enabled is the explicit test-instance
// opt-in, applied only after validation while the production refusal remains.
func (h *qualificationHarness) ReloadInstanceConfig(edit func(string), enabled bool) string {
	edit(h.configFile)
	return h.reloadConfig(enabled)
}

func (h *qualificationHarness) reloadInstanceConfig(enabled bool) *instance.Config {
	cfg, err := instance.LoadConfig(h.configFile)
	if err != nil {
		h.t.Fatal(err)
	}
	if cfg.Engine == nil {
		cfg.Engine = &instance.EngineConfig{}
	}
	cfg.Engine.ChildWorkflows = &instance.EngineChildWorkflowsConfig{Enabled: enabled}
	return cfg
}

// History proves the happy path crossed the production dispatch activity and
// transported its frozen enable pin and trusted handoff, not a fixture Invoke.
func (h *qualificationHarness) assertProductionChildHandoff(run client.WorkflowRun) {
	h.t.Helper()
	history := h.temporal.Client().GetWorkflowHistory(h.ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	dispatches := map[int64]bool{}
	handoffs := 0
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			h.t.Fatal(err)
		}
		if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil && scheduled.ActivityType.Name == engine.ActDispatchStage {
			var input engine.DispatchStageInput
			if err := converter.GetDefaultDataConverter().FromPayloads(scheduled.Input, &input); err != nil {
				h.t.Fatal(err)
			}
			if !input.ChildWorkflowsEnabled || input.Envelope.ChildWorkflowOrigin == nil {
				h.t.Fatal("production dispatch lost frozen child authority")
			}
			dispatches[event.EventId] = true
		}
		if completed := event.GetActivityTaskCompletedEventAttributes(); completed != nil && dispatches[completed.ScheduledEventId] {
			var result engine.DispatchStageResult
			if err := converter.GetDefaultDataConverter().FromPayloads(completed.Result, &result); err != nil {
				h.t.Fatal(err)
			}
			if result.ChildWait != nil {
				handoffs++
			}
		}
	}
	if len(dispatches) != 2 || handoffs != 1 {
		h.t.Fatalf("production dispatch history: %d invocations, %d handoffs", len(dispatches), handoffs)
	}
}
