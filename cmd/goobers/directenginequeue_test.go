package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginestartintent"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type directHistoryClient struct {
	client.Client
	t            *testing.T
	dc           converter.DataConverter
	starts       int
	dials        int
	lost         bool
	hide         bool
	extraHistory []*historypb.HistoryEvent
	input        engine.RunInput
	started      *historypb.WorkflowExecutionStartedEventAttributes
	before       func(engine.RunInput)
}
type directHistoryRun struct {
	client.WorkflowRun
	id string
}

func (r directHistoryRun) GetID() string { return r.id }
func (c *directHistoryClient) Close()    {}
func (c *directHistoryClient) WorkflowService() workflowservice.WorkflowServiceClient {
	return directHistoryService{owner: c}
}

type directHistoryService struct {
	workflowservice.WorkflowServiceClient
	owner *directHistoryClient
}

func (s directHistoryService) GetWorkflowExecutionHistory(ctx context.Context, request *workflowservice.GetWorkflowExecutionHistoryRequest, opts ...grpc.CallOption) (*workflowservice.GetWorkflowExecutionHistoryResponse, error) {
	return s.owner.GetWorkflowExecutionHistory(ctx, request, opts...)
}
func (c *directHistoryClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	c.starts++
	in := args[0].(engine.RunInput)
	if c.before != nil {
		c.before(in)
	}
	c.input = in
	payload, err := c.dc.ToPayloads(in)
	if err != nil {
		return nil, err
	}
	memo := &commonpb.Memo{Fields: map[string]*commonpb.Payload{}}
	for key, value := range opts.Memo {
		p, err := c.dc.ToPayload(value)
		if err != nil {
			return nil, err
		}
		memo.Fields[key] = p
	}
	c.started = &historypb.WorkflowExecutionStartedEventAttributes{Input: payload, Memo: memo, TaskQueue: &taskqueuepb.TaskQueue{Name: opts.TaskQueue}, WorkflowType: &commonpb.WorkflowType{Name: "Run"}}
	if c.lost {
		return nil, errors.New("reply lost after actual provider start")
	}
	return directHistoryRun{id: in.RunID}, nil
}
func (c *directHistoryClient) GetWorkflowExecutionHistory(_ context.Context, request *workflowservice.GetWorkflowExecutionHistoryRequest, _ ...grpc.CallOption) (*workflowservice.GetWorkflowExecutionHistoryResponse, error) {
	if request.Namespace != "private" || request.MaximumPageSize != 1 || request.Execution.WorkflowId != c.input.RunID {
		c.t.Errorf("unbound history request: %+v", request)
	}
	if c.hide || c.started == nil {
		return nil, serviceerror.NewNotFound("history not available")
	}
	events := []*historypb.HistoryEvent{{EventId: 1, Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{WorkflowExecutionStartedEventAttributes: c.started}}}
	events = append(events, c.extraHistory...)
	return &workflowservice.GetWorkflowExecutionHistoryResponse{History: &historypb.History{Events: events}}, nil
}
func installDirectHistoryClient(t *testing.T, c *directHistoryClient) {
	t.Helper()
	c.t = t
	previous := dialDirectEngine
	t.Cleanup(func() { dialDirectEngine = previous })
	dialDirectEngine = func(_ context.Context, host, namespace string, _ *temporaldial.TLS, dc ...converter.DataConverter) (client.Client, error) {
		if host != "pinned.example:7233" || namespace != "private" || len(dc) != 1 {
			t.Fatal(host, namespace, len(dc))
		}
		c.dials++
		c.dc = dc[0]
		return c, nil
	}
}
func directArgs(root string) []string {
	return []string{"--direct", "--temporal-hostport", "pinned.example:7233", "--temporal-namespace", "private", "--task-queue", "exact-workers", "--dedupe-key", "one", "default-implement", root}
}

func TestDirectEngineCLIPersistsExactInputBeforeProviderAndReplaysAcrossSourceChange(t *testing.T) {
	root := initDeterministicDemo(t)
	// A real first persistence batch commonly includes workflow task scheduling.
	c := &directHistoryClient{extraHistory: []*historypb.HistoryEvent{{EventId: 2, Attributes: &historypb.HistoryEvent_WorkflowTaskScheduledEventAttributes{WorkflowTaskScheduledEventAttributes: &historypb.WorkflowTaskScheduledEventAttributes{}}}}}
	installDirectHistoryClient(t, c)
	queue := directTestQueue(t, root)
	c.before = func(in engine.RunInput) {
		page, err := queue.DirectEnginePage(t.Context(), "")
		if err != nil || len(page) != 1 || page[0].State != triggerqueue.Dispatching {
			t.Fatal(page, err)
		}
		e, err := enginestartintent.Parse(page[0].Payload)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := queue.DirectEngineInput(t.Context(), page[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		captured, _ := json.Marshal(in)
		if !bytes.Equal(captured, raw) || e.InputDigest != enginestartintent.Digest(raw) {
			t.Fatal("provider preceded exact durable input")
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runEngineStart(directArgs(root), &stdout, &stderr); code != 0 {
		t.Fatal(code, stdout.String(), stderr.String())
	}
	want := engine.RunID("example", "default-implement", "one")
	if c.starts != 1 || !strings.Contains(stdout.String(), "engine run started: "+want) || c.started.Memo.Fields[engine.RunInputDigestMemoKey] == nil {
		t.Fatal(c.starts, stdout.String())
	}
	source := filepath.Join(root, "config/gaggles/example/workflows/default-implement.yaml")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	writeFileContent(t, source, string(raw)+"\n# another authored version\n")
	stdout.Reset()
	stderr.Reset()
	if code := runEngineStart(directArgs(root), &stdout, &stderr); code != 0 || c.starts != 1 || c.dials != 1 || !strings.Contains(stdout.String(), want) {
		t.Fatal(code, c.starts, c.dials, stdout.String(), stderr.String())
	}
	writeFileContent(t, source, "invalid pending source: [")
	explicit := append([]string{"--gaggle", "example"}, directArgs(root)...)
	if code := runEngineStart(explicit, &stdout, &stderr); code != 0 || c.starts != 1 || c.dials != 1 {
		t.Fatal("accepted replay depended on mutable source", code, c.starts, c.dials, stderr.String())
	}
	args := directArgs(root)
	args[6] = "another-queue"
	args = append([]string{"--gaggle", "example"}, args...)
	if code := runEngineStart(args, &stdout, &stderr); code == 0 || c.starts != 1 {
		t.Fatal("changed queue was accepted", code, c.starts)
	}
}

func TestDirectEngineDaemonReconcilesLostReplyWithConfiguredSealedCodec(t *testing.T) {
	root := initDeterministicDemo(t)
	cfg := configureTestTemporalCodec(t, root)
	layout := instance.NewLayout(root)
	c := &directHistoryClient{lost: true}
	installDirectHistoryClient(t, c)
	var stdout, stderr bytes.Buffer
	if code := runEngineStart(directArgs(root), &stdout, &stderr); code != 1 || c.starts != 1 {
		t.Fatal(code, c.starts, stderr.String())
	}
	assertSealedConverter(t, c.dc)
	service, err := newDurableTriggerService(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.queue.Close() })
	service.directEngine = directEngineService(layout, service.queue, cfg)
	page, err := service.queue.DirectEnginePage(t.Context(), "")
	if err != nil || len(page) != 1 || page[0].State != triggerqueue.Dispatching {
		t.Fatal(page, err)
	}
	record := page[0]
	e, err := enginestartintent.Parse(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := retainedExecutionGenerationPins(t.Context(), layout)
	if err != nil || !pins[e.ConfigGeneration] {
		t.Fatal(pins, err)
	}
	c.hide = true
	if err = service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := service.queue.Get(t.Context(), record.ID, enginestartintent.Actor)
	if err != nil || current.State != triggerqueue.Dispatching || c.starts != 1 {
		t.Fatal(current, err, c.starts)
	}
	c.hide = false
	service.directEngineCursor = ""
	if err = service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err = service.queue.Get(t.Context(), record.ID, enginestartintent.Actor)
	if err != nil || current.State != triggerqueue.Dispatched || current.RunID != c.input.RunID || c.starts != 1 {
		t.Fatal(current, err, c.starts)
	}
}

func TestDirectEngineBindingChangeRefusesBeforeEffectAndWrongHistoryStaysUncertain(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	q := directTestQueue(t, root)
	service := directEngineService(layout, q, cfg)
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := enginestartintent.CredentialBinding(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := enginestartintent.Request{HostPort: "pinned.example:7233", Namespace: "private", TaskQueue: "exact-workers", Gaggle: "example", Workflow: "default-implement", DedupeKey: "one", Directory: directory, Binding: binding}
	record, _, err := service.Accept(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	c := &directHistoryClient{lost: true}
	installDirectHistoryClient(t, c)
	changed := *cfg
	changed.Engine = &instance.EngineConfig{TLS: &temporaldial.TLS{ServerName: "another-host"}}
	changedService := directEngineService(layout, q, &changed)
	if err = changedService.Dispatch(t.Context(), record); err == nil || c.dials != 0 || c.starts != 0 {
		t.Fatal(err, c.dials, c.starts)
	}
	if err = service.Dispatch(t.Context(), record); !errors.Is(err, enginestartintent.ErrUncertain) {
		t.Fatal(err)
	}
	record, err = q.Get(t.Context(), record.ID, enginestartintent.Actor)
	if err != nil {
		t.Fatal(err)
	}
	original := proto.Clone(c.started).(*historypb.WorkflowExecutionStartedEventAttributes)
	for _, change := range []string{"queue", "workflow", "input", "unknown input field", "memo", "missing input"} {
		err = nil
		c.started = proto.Clone(original).(*historypb.WorkflowExecutionStartedEventAttributes)
		switch change {
		case "queue":
			c.started.TaskQueue.Name = "wrong-queue"
		case "workflow":
			c.started.WorkflowType.Name = "UnrelatedWorkflow"
		case "input":
			input := c.input
			input.RunID = "another-run"
			c.started.Input, err = c.dc.ToPayloads(input)
		case "unknown input field":
			raw, marshalErr := json.Marshal(c.input)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			raw = append(raw[:len(raw)-1], []byte(`,"unknownWorkerAuthority":true}`)...)
			c.started.Input, err = c.dc.ToPayloads(json.RawMessage(raw))
		case "memo":
			c.started.Memo.Fields[engine.RunInputDigestMemoKey], err = c.dc.ToPayload("sha256:another-input")
		case "missing input":
			c.started.Input = nil
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = service.Dispatch(t.Context(), record); err == nil || c.starts != 1 {
			t.Fatal(change, "was acknowledged", err, c.starts)
		}
	}
	// A legacy history may lack the new memo, but its actual original input
	// and task queue still have to verify through the configured converter.
	c.started = original
	c.extraHistory = make([]*historypb.HistoryEvent, 4096)
	if err = service.Dispatch(t.Context(), record); err == nil || c.starts != 1 {
		t.Fatal("oversized event page was acknowledged", err, c.starts)
	}
	c.extraHistory = []*historypb.HistoryEvent{{EventId: 2, Attributes: &historypb.HistoryEvent_WorkflowTaskScheduledEventAttributes{WorkflowTaskScheduledEventAttributes: &historypb.WorkflowTaskScheduledEventAttributes{}}}}
	c.started.Memo = nil
	if err = service.Dispatch(t.Context(), record); err != nil || c.starts != 1 {
		t.Fatal(err, c.starts)
	}
}

func directTestQueue(t *testing.T, root string) *triggerqueue.Store {
	t.Helper()
	queue, err := triggerqueue.Open(filepath.Join(instance.NewLayout(root).SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	return queue
}
