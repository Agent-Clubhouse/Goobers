package temporalcodec

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/temporaltest"
)

func codecHistoryWorkflow(ctx workflow.Context, in apiv1.InvocationEnvelope) (apiv1.InvocationEnvelope, error) {
	var proceed string
	workflow.GetSignalChannel(ctx, "proceed").Receive(ctx, &proceed)
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	var out apiv1.InvocationEnvelope
	err := workflow.ExecuteActivity(ctx, codecHistoryActivity, in).Get(ctx, &out)
	return out, err
}
func codecHistoryActivity(_ context.Context, in apiv1.InvocationEnvelope) (apiv1.InvocationEnvelope, error) {
	return in, nil
}

// Uses an actual SDK client, worker and ephemeral server. In the mixed case an
// old client writes the start/signal before an upgraded worker processes it.
func TestSDKSealedHistoryAndLegacyMixedReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	}()
	raw := server.Client()
	cfg := fileCodecConfig(t)
	dc, err := DataConverter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sealedClient, err := temporaldial.Dial(ctx, server.FrontendHostPort(), "default", nil, dc)
	if err != nil {
		t.Fatal(err)
	}
	defer sealedClient.Close()
	for _, mode := range []string{"legacy", "sealed", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			writer, processor := raw, raw
			if mode == "sealed" {
				writer = sealedClient
			}
			if mode != "legacy" {
				processor = sealedClient
			}
			queue := "codec-history-" + mode
			input := apiv1.InvocationEnvelope{TaskID: "private-task", WorkflowID: "private-workflow", RunID: "private-run", Gaggle: "private-gaggle", Inputs: map[string]any{"instructions": "private-canary-goal-and-instructions"}}
			run, err := writer.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue, Memo: map[string]any{"gaggle": "private-gaggle"}}, codecHistoryWorkflow, input)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "proceed", "private-signal"); err != nil {
				t.Fatal(err)
			}
			w := worker.New(processor, queue, worker.Options{})
			w.RegisterWorkflow(codecHistoryWorkflow)
			w.RegisterActivity(codecHistoryActivity)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			var result apiv1.InvocationEnvelope
			// Read the result through the configured client even when an old writer
			// started the workflow; this also verifies plaintext legacy result reads.
			if err := sealedClient.GetWorkflow(ctx, run.GetID(), run.GetRunID()).Get(ctx, &result); err != nil {
				t.Fatal(err)
			}
			if result.Inputs["instructions"] != input.Inputs["instructions"] {
				t.Fatal("decoded result differs")
			}
			history := &historypb.History{}
			iter := raw.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
			sealedCount, plainCount := 0, 0
			check := func(payloads []*commonpb.Payload) {
				for _, p := range payloads {
					if string(p.Metadata["encoding"]) == Encoding {
						sealedCount++
						encoded, _ := proto.Marshal(p)
						if bytes.Contains(encoded, []byte("private-")) {
							t.Fatal("sensitive content present in sealed raw history")
						}
						if string(p.Metadata[keyVersionField]) != "v1" {
							t.Fatal("missing backend key version")
						}
					} else {
						plainCount++
					}
				}
			}
			for iter.HasNext() {
				e, err := iter.Next()
				if err != nil {
					t.Fatal(err)
				}
				history.Events = append(history.Events, e)
				switch {
				case e.GetWorkflowExecutionStartedEventAttributes() != nil:
					a := e.GetWorkflowExecutionStartedEventAttributes()
					check(a.GetInput().GetPayloads())
					for _, p := range a.GetMemo().GetFields() {
						check([]*commonpb.Payload{p})
					}
				case e.GetWorkflowExecutionSignaledEventAttributes() != nil:
					check(e.GetWorkflowExecutionSignaledEventAttributes().GetInput().GetPayloads())
				case e.GetActivityTaskScheduledEventAttributes() != nil:
					check(e.GetActivityTaskScheduledEventAttributes().GetInput().GetPayloads())
				case e.GetActivityTaskCompletedEventAttributes() != nil:
					check(e.GetActivityTaskCompletedEventAttributes().GetResult().GetPayloads())
				case e.GetWorkflowExecutionCompletedEventAttributes() != nil:
					check(e.GetWorkflowExecutionCompletedEventAttributes().GetResult().GetPayloads())
				}
			}
			switch mode {
			case "legacy":
				if plainCount < 6 || sealedCount != 0 {
					t.Fatalf("legacy counts sealed=%d plain=%d", sealedCount, plainCount)
				}
			case "sealed":
				if sealedCount < 6 || plainCount != 0 {
					t.Fatalf("sealed counts sealed=%d plain=%d", sealedCount, plainCount)
				}
			case "mixed":
				if sealedCount < 3 || plainCount < 3 {
					t.Fatalf("mixed counts sealed=%d plain=%d", sealedCount, plainCount)
				}
			}
			replayer, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{DataConverter: dc})
			if err != nil {
				t.Fatal(err)
			}
			replayer.RegisterWorkflow(codecHistoryWorkflow)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatalf("codec replay of %s: %v", mode, err)
			}
			strictCodec, err := FromConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			strictCodec.strict = true
			strictDC := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), strictCodec)
			strictReplayer, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{DataConverter: strictDC})
			if err != nil {
				t.Fatal(err)
			}
			strictReplayer.RegisterWorkflow(codecHistoryWorkflow)
			err = strictReplayer.ReplayWorkflowHistory(nil, history)
			if (err == nil) != (mode == "sealed") {
				t.Fatalf("strict replay %s: %v", mode, err)
			}
		})
	}
}
