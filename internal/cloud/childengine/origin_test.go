package childengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginechildplane"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
)

type originTemporalFake struct {
	description *workflowservice.DescribeWorkflowExecutionResponse
	events      []*historypb.HistoryEvent
	historyErr  error
}

func (f *originTemporalFake) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return f.description, nil
}
func (f *originTemporalFake) GetWorkflowHistory(context.Context, string, string, bool, enumspb.HistoryEventFilterType) client.HistoryEventIterator {
	return &originHistory{events: f.events, err: f.historyErr}
}

type originHistory struct {
	events []*historypb.HistoryEvent
	err    error
}

func (h *originHistory) HasNext() bool { return h.err != nil || len(h.events) > 0 }
func (h *originHistory) Next() (*historypb.HistoryEvent, error) {
	if h.err != nil {
		err := h.err
		h.err = nil
		return nil, err
	}
	e := h.events[0]
	h.events = h.events[1:]
	return e, nil
}

func originActivityFixture(t *testing.T) (*originTemporalFake, acceptance.RunPins, livejournal.ChildOriginEmitRequest) {
	t.Helper()
	pins := acceptance.RunPins{RunID: "run-origin", Gaggle: "web", AcceptanceID: "accepted-origin", InputDigest: journal.Digest([]byte("input"))}
	now := time.Now().UTC()
	batch := livejournal.EmitRequest{RunID: pins.RunID, Gaggle: pins.Gaggle, Open: &livejournal.OpenHeader{Identity: journal.RunIdentity{RunID: pins.RunID, Gaggle: pins.Gaggle, Workflow: "wf", WorkflowVersion: 1, WorkflowDigest: journal.Digest([]byte("workflow")), Trigger: journal.Trigger{Kind: journal.TriggerManual}}, Graph: []byte(`{"nodes":[]}`), Definition: []byte(`{"name":"wf"}`)}, Ops: []livejournal.Op{
		{Kind: livejournal.OpAppend, Key: "run-start", Time: now, Event: &journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}},
		{Kind: livejournal.OpAppend, Key: "stage-start", Time: now.Add(time.Second), ChildStageStart: &livejournal.ChildStageStart{}, Event: &journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}},
		{Kind: livejournal.OpAppend, Key: "after-start", Time: now.Add(2 * time.Second), Event: &journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "after-start"}}},
	}}
	dc := converter.GetDefaultDataConverter()
	payload, err := dc.ToPayload(startfence.StartOwnerMemo{AcceptanceID: pins.AcceptanceID, InputDigest: pins.InputDigest})
	if err != nil {
		t.Fatal(err)
	}
	input, err := dc.ToPayloads(batch)
	if err != nil {
		t.Fatal(err)
	}
	fake := &originTemporalFake{description: &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: pins.RunID, RunId: "execution-1"}, Type: &commonpb.WorkflowType{Name: "Run"}, Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{startfence.StartOwnerMemoKey: payload}}}, PendingActivities: []*workflowpb.PendingActivityInfo{{ActivityId: "journal-1", ActivityType: &commonpb.ActivityType{Name: engine.ActEmitJournal}, Attempt: 1, State: enumspb.PENDING_ACTIVITY_STATE_STARTED, LastWorkerIdentity: "resident-1"}}}, events: []*historypb.HistoryEvent{{Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{ActivityId: "journal-1", ActivityType: &commonpb.ActivityType{Name: engine.ActEmitJournal}, Input: input}}}}}
	return fake, pins, livejournal.ChildOriginEmitRequest{Activity: livejournal.ChildOriginActivity{WorkflowRunID: "execution-1", ActivityID: "journal-1", Attempt: 1}, Batch: batch}
}

func TestChildOriginRequiresExactResidentActivityLoan(t *testing.T) {
	for _, scenario := range []string{"valid", "other-worker", "other-run", "stale-attempt", "reset-execution", "changed-batch", "history-error", "scheduled-not-started"} {
		t.Run(scenario, func(t *testing.T) {
			temporal, pins, request := originActivityFixture(t)
			switch scenario {
			case "other-worker":
				temporal.description.PendingActivities[0].LastWorkerIdentity = "resident-2"
			case "other-run":
				request.Batch.RunID = "run-other"
			case "stale-attempt":
				request.Activity.Attempt++
			case "reset-execution":
				temporal.description.WorkflowExecutionInfo.Execution.RunId = "execution-2"
			case "changed-batch":
				request.Batch.Ops[1].Event.Stage = "forged-stage"
			case "history-error":
				temporal.historyErr = errors.New("history unavailable")
			case "scheduled-not-started":
				temporal.description.PendingActivities[0].State = enumspb.PENDING_ACTIVITY_STATE_SCHEDULED
			}
			err := verifyOriginActivity(t.Context(), temporal, pins, "worker:resident-1", request)
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			want := engine.ErrChildCustodyRefused
			if scenario == "history-error" {
				want = engine.ErrChildCustodyUnavailable
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: %v", scenario, err)
			}
		})
	}
}

type originHTTPAuthority struct {
	engine.ChildWorkflowAuthority
	temporal   *originTemporalFake
	pins       acceptance.RunPins
	writer     *livejournal.Writer
	failPrefix bool
}

func (a *originHTTPAuthority) EmitChildOrigin(ctx context.Context, worker string, request livejournal.ChildOriginEmitRequest) (livejournal.EmitResponse, error) {
	if err := verifyOriginActivity(ctx, a.temporal, a.pins, worker, request); err != nil {
		return livejournal.EmitResponse{}, err
	}
	if a.failPrefix {
		a.failPrefix = false
		prefix := request.Batch
		prefix.Ops = prefix.Ops[:1]
		if _, err := a.writer.Emit(ctx, prefix); err != nil {
			return livejournal.EmitResponse{}, err
		}
		return livejournal.EmitResponse{}, engine.ErrChildCustodyUnavailable
	}
	return a.writer.Emit(ctx, request.Batch)
}

func TestChildOriginSignedHTTPReplaysWholeBatchWithoutReordering(t *testing.T) {
	temporal, pins, request := originActivityFixture(t)
	dir := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return dir, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	backend := &originHTTPAuthority{temporal: temporal, pins: pins, writer: writer, failPrefix: true}
	plane, err := enginechildplane.NewAuthority(backend)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(struct{ readservice.Reader }{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithEngineChildAuthority(plane), httpapi.WithJournalService(writer))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	emitter := &livejournal.HTTPEmitter{BaseURL: server.URL, Minter: signer, ChildOriginWorkerID: "resident-1"}
	ctx := livejournal.WithChildOriginActivity(t.Context(), request.Activity)
	if _, err := emitter.Emit(ctx, request.Batch); err == nil {
		t.Fatal("injected failure missing")
	}
	first, err := emitter.Emit(ctx, request.Batch)
	if err != nil || len(first.ChildOrigins) != 1 {
		t.Fatalf("first=%+v %v", first, err)
	}
	again, err := emitter.Emit(ctx, request.Batch)
	if err != nil || again.Applied != 0 || len(again.ChildOrigins) != 1 || again.ChildOrigins[0] != first.ChildOrigins[0] {
		t.Fatalf("replay=%+v %v", again, err)
	}
	reader, err := journal.OpenRead(filepath.Join(dir, pins.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 3 || events[0].Type != journal.EventRunStarted || events[1].Type != journal.EventStageStarted || events[2].Type != journal.EventRunnerAnnotation {
		t.Fatalf("reordered journal=%+v %v", events, err)
	}
	origin, err := journal.ChildWorkflowOriginForEvent(pins.RunID, events[1])
	if err != nil || *origin != first.ChildOrigins[0].Origin {
		t.Fatalf("origin not writer-derived: %+v %v", origin, err)
	}
	// The same body cannot escape the workload guard on the ordinary route.
	pod, _ := signer.Mint(pins.RunID, time.Minute)
	emitter.ChildOriginWorkerID, emitter.Token = "", pod
	if _, err := emitter.Emit(ctx, request.Batch); err == nil {
		t.Fatal("pod bound an origin")
	}
	// Workload and other worker credentials cannot enter the confined seam.
	childToken, _ := signer.MintChildPod(pins.RunID, journal.Digest(nil), time.Minute)
	parentToken, _ := signer.MintWorkflowParentPod(pins.RunID, journal.Digest(nil), time.Minute)
	blobToken, _ := signer.MintWorkerBlob("resident-1", time.Minute)
	body, _ := json.Marshal(request)
	for _, token := range []string{pod, childToken, parentToken, blobToken} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/engine/children/origin", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign credential admitted: HTTP %d", response.StatusCode)
		}
	}
	// A genuine peer credential does not inherit this resident's activity loan.
	emitter.ChildOriginWorkerID, emitter.Token = "resident-2", ""
	if _, err := emitter.Emit(ctx, request.Batch); err == nil {
		t.Fatal("peer worker borrowed another activity")
	}
}
