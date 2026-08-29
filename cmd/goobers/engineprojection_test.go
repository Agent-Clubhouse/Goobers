package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// noDaemonEngineClient asserts that cfg builds no shared Temporal client at
// all — the dial seam fails the test if it is reached — and returns the nil
// client the daemon would thread into every engine consumer.
func noDaemonEngineClient(t *testing.T, cfg *instance.Config) *daemonEngineClient {
	t.Helper()
	previousDial := dialDaemonEngine
	dialDaemonEngine = func(string, string) (client.Client, error) {
		return nil, errors.New("daemon dialed Temporal unexpectedly")
	}
	t.Cleanup(func() { dialDaemonEngine = previousDial })

	engineClient, err := newDaemonEngineClient(cfg)
	if err != nil {
		t.Fatalf("newDaemonEngineClient: %v", err)
	}
	if engineClient != nil {
		t.Fatal("newDaemonEngineClient built a client for an instance with no engine configuration")
	}
	return engineClient
}

func TestEngineProjectionIsInertWithoutTemporalConfiguration(t *testing.T) {
	root := t.TempDir()
	cfg := &instance.Config{}
	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), cfg, nil, noDaemonEngineClient(t, cfg), nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
}

func TestEngineProjectionIsInertWithNamespaceAndTaskQueueOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv(instance.TemporalNamespaceEnv, "production")
	t.Setenv(instance.TaskQueueEnv, "production")
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte("apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	// noDaemonEngineClient carries the assertion the loop's own
	// dialEngineProjection seam used to make: nothing dials. It now covers
	// more, because the daemon's single dial is the only one left.
	stop, err := startEngineProjection(context.Background(), instance.NewLayout(root), cfg, nil, noDaemonEngineClient(t, cfg), nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()
}

// countingTemporalClient is a client.Client that answers the only call the
// projection reconciler makes on its first tick and inherits panics for
// everything else, so a consumer that starts using the shared connection for
// something new has to say so here.
type countingTemporalClient struct {
	client.Client
	closes int
}

func (c *countingTemporalClient) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return &workflowservice.ListWorkflowExecutionsResponse{}, nil
}

func (c *countingTemporalClient) Close() { c.closes++ }

// TestDaemonDialsTemporalOnceForEveryEngineConsumer is decision 003 step 1(e)'s
// headline claim, asserted rather than restated: the projection reconciler,
// the DS6 claim-liveness probe and the engine-driven run guards share ONE
// connection to the frontend. Each used to dial its own — three TCP
// connections, three failure modes, three places an operator has to look — and
// nothing but this test stops a future change from re-dialing inside any of
// the three, because each consumer still builds fine from its own dial.
//
// The dial seam is the whole surface: dialDaemonEngine is the only entry
// point, so counting it counts connections.
func TestDaemonDialsTemporalOnceForEveryEngineConsumer(t *testing.T) {
	root := initDeterministicDemo(t)
	configureEngineInstance(t, root)
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}

	var dials int
	shared := &countingTemporalClient{}
	previousDial := dialDaemonEngine
	dialDaemonEngine = func(string, string) (client.Client, error) {
		dials++
		return shared, nil
	}
	t.Cleanup(func() { dialDaemonEngine = previousDial })

	engineClient, err := newDaemonEngineClient(cfg)
	if err != nil {
		t.Fatalf("newDaemonEngineClient: %v", err)
	}
	if engineClient == nil {
		t.Fatal("engine-configured instance built no shared Temporal client")
	}

	// Consumer 1: the completed-run projection reconciler.
	stop, err := startEngineProjection(context.Background(), l, cfg, set, engineClient, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	stop()

	// Consumer 2: the DS6 claim-liveness probe.
	probe, closeProbe, err := buildClaimLivenessProbe(cfg, engineClient, func() []string { return nil })
	if err != nil {
		t.Fatalf("buildClaimLivenessProbe: %v", err)
	}
	if probe == nil {
		t.Fatal("engine-configured instance built no claim liveness probe")
	}
	closeProbe()

	// Consumer 3: the engine-driven run guards.
	if engineClient.Guards() == nil {
		t.Fatal("engine-configured instance built no run guards")
	}

	if dials != 1 {
		t.Fatalf("dialDaemonEngine called %d times, want exactly 1 — the three engine consumers must share the daemon's one connection", dials)
	}
	// A consumer never closes what it borrows; `goobers up` owns the lifetime.
	if shared.closes != 0 {
		t.Fatalf("shared client closed %d times by its consumers, want 0 — only the daemon that dialed it may close it", shared.closes)
	}
	engineClient.Close()
	if shared.closes != 1 {
		t.Fatalf("shared client closed %d times by its owner, want 1", shared.closes)
	}
}

// The JSON wire values of internal/engine's journal op kinds (journal.go's
// opAppend/opSpan). A projection crosses the Temporal query boundary as data,
// so these strings are the contract this test stands on.
const (
	projOpAppend = "append"
	projOpSpan   = "span"
)

// transcriptRunProjection is the history projection of a completed run whose
// implement stage produced a harness transcript: the workflow records a
// POINTER-ONLY span op (it never holds the bytes — internal/engine/journal.go
// JournalSpanOp), so whether the transcript survives projection is entirely a
// question of what span source the daemon wired into the reconciler.
func transcriptRunProjection(runID, gaggle, digest string, at time.Time) engine.JournalProjection {
	return engine.JournalProjection{
		Identity: journal.RunIdentity{
			RunID: runID, Workflow: "implementation", WorkflowVersion: 1,
			WorkflowDigest: "sha256:abc", Gaggle: gaggle,
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		},
		Graph:      json.RawMessage(`{"nodes":[]}`),
		Definition: json.RawMessage(`{"name":"implementation"}`),
		Ops: []engine.JournalOp{
			{Kind: projOpAppend, Time: at, Event: &journal.Event{
				Type: journal.EventRunStarted, Status: string(journal.PhaseRunning),
			}},
			{Kind: projOpSpan, Time: at.Add(time.Second), Span: &engine.JournalSpanOp{
				Stage: "implement", Attempt: 1, Name: "implement.transcript",
				DataSchema: "goobers.dev/telemetry/genai-event/v1",
				Ref:        journal.Ref{Digest: digest},
			}},
			{Kind: projOpAppend, Time: at.Add(2 * time.Second), Event: &journal.Event{
				Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
			}},
		},
	}
}

// projectionEncodedValue is what a Temporal journal query returns.
type projectionEncodedValue struct {
	proj engine.JournalProjection
}

func (v projectionEncodedValue) HasValue() bool { return true }

func (v projectionEncodedValue) Get(valuePtr interface{}) error {
	out, ok := valuePtr.(*engine.JournalProjection)
	if !ok {
		return errors.New("unexpected projection destination")
	}
	*out = v.proj
	return nil
}

// completedRunClientStub is the engineProjectionClient the daemon hands the
// loop: one closed execution carrying its gaggle memo, whose journal query
// answers with proj. It is deliberately the NARROW surface
// (engine.CompletedRunClient) rather than a whole temporal client.Client,
// which is what makes the daemon's own projection wiring drivable at all. It
// still records Close so the test can assert the loop does NOT call it — the
// connection is borrowed from `goobers up`, not owned here.
type completedRunClientStub struct {
	mu         sync.Mutex
	proj       engine.JournalProjection
	executions []*workflowpb.WorkflowExecutionInfo
	queries    int
	closed     bool
}

func newCompletedRunClientStub(t *testing.T, proj engine.JournalProjection) *completedRunClientStub {
	t.Helper()
	payload, err := converter.GetDefaultDataConverter().ToPayload(proj.Identity.Gaggle)
	if err != nil {
		t.Fatal(err)
	}
	return &completedRunClientStub{
		proj: proj,
		executions: []*workflowpb.WorkflowExecutionInfo{{
			Execution: &commonpb.WorkflowExecution{WorkflowId: proj.Identity.RunID},
			Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{engine.RunGaggleMemoKey: payload}},
		}},
	}
}

func (c *completedRunClientStub) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return &workflowservice.ListWorkflowExecutionsResponse{Executions: c.executions}, nil
}

func (c *completedRunClientStub) QueryWorkflow(context.Context, string, string, string, ...interface{}) (converter.EncodedValue, error) {
	c.mu.Lock()
	c.queries++
	c.mu.Unlock()
	return projectionEncodedValue{proj: c.proj}, nil
}

func (c *completedRunClientStub) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// TestEngineProjectionAdoptsSpansFromTheDaemonBlobStore is the RECONCILER
// WIRING seam for #3805 — startEngineProjection itself, not the option it
// applies.
//
// engine's own tests prove a reconciler holding a span source adopts and
// verifies spans; what was broken was that the daemon never handed it one, so
// the repair/backfill projection wrote a conformance-normative
// span_unavailable error event in place of every pod-executed stage's
// transcript. This test drives the daemon's loop with a fake Temporal client
// and the daemon's own blob store, and reads what landed on disk.
func TestEngineProjectionAdoptsSpansFromTheDaemonBlobStore(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}}
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q"}}

	// The daemon's own store, at the layout path up.go constructs it from —
	// and the store a stage pod PUTs its scrubbed transcript into over the
	// blob plane before the workflow's pointer-only span op is projected.
	blobs, err := blobstore.NewDir(layout.BlobStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	transcript := []byte(`{"event":"prompt","adapter":"copilot-cli"}`)
	digest := journal.Digest(transcript)
	if err := blobs.Put(context.Background(), digest, transcript); err != nil {
		t.Fatalf("seed blob store: %v", err)
	}

	const runID = "projection-span-run"
	at := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	stub := newCompletedRunClientStub(t, transcriptRunProjection(runID, "web", digest, at))

	previousClient := projectionTemporalClient
	projectionTemporalClient = func(*daemonEngineClient) engineProjectionClient { return stub }
	t.Cleanup(func() { projectionTemporalClient = previousClient })

	stop, err := startEngineProjection(context.Background(), layout, cfg, set, nil, nil, nil, nil, nil, blobs)
	if err != nil {
		t.Fatalf("startEngineProjection: %v", err)
	}
	runDir := filepath.Join(layout.ForGaggle("web").RunsDir(), runID)
	waitForRecordedRun(t, runDir)
	stop()

	rd, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var span *journal.Event
	for i := range events {
		if events[i].Type == journal.EventSpanRecorded {
			span = &events[i]
		}
		if events[i].Type == journal.EventError && events[i].Error != nil {
			t.Fatalf("the daemon's projection could not adopt the span: %+v", events[i].Error)
		}
	}
	if span == nil || span.Ref == nil {
		t.Fatalf("projected run has no span.recorded event; events = %+v", events)
	}
	got, err := rd.SpanBytes(*span.Ref)
	if err != nil {
		t.Fatalf("SpanBytes: %v", err)
	}
	if string(got) != string(transcript) {
		t.Fatalf("projected span bytes = %q, want %q", got, transcript)
	}
	// The loop must NOT close the connection it borrows: since decision 003
	// step 1(e) the same client serves the DS6 liveness probe and the
	// engine-driven run guards, and `goobers up` closes it once at shutdown
	// (asserted in TestDaemonDialsTemporalOnceForEveryEngineConsumer).
	if stub.closed {
		t.Fatal("the projection loop closed the daemon's shared Temporal client, taking the liveness probe and run guards down with it")
	}
}

func waitForRecordedRun(t *testing.T, runDir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if journal.Recorded(runDir) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the projection loop never published %s", runDir)
}
