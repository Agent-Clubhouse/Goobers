package enginestartintent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// TemporalBackend uses one configured client and codec for start and observation.
// History is read in a single first-event page; no visibility scan is involved.
type TemporalBackend struct {
	Client    client.Client
	Namespace string
	TaskQueue string
	Converter converter.DataConverter
}

// Close releases the exact frontend connection.
func (b *TemporalBackend) Close() { b.Client.Close() }

// Start stamps the durable input digest alongside the engine's ordinary memos.
func (b *TemporalBackend) Start(ctx context.Context, in engine.RunInput, digest string) error {
	result, err := engine.NewTemporalStarter(b.Client, b.TaskQueue).StartWithInputDigest(ctx, in, digest)
	if err == nil && result.RunID != in.RunID {
		return errors.New("direct engine: provider returned another execution identity")
	}
	return err
}

// Observe verifies the actual first event, including legacy histories without
// the new memo. A missing history remains an unproven, potentially applied effect.
func (b *TemporalBackend) Observe(ctx context.Context, in engine.RunInput, digest string) (bool, error) {
	response, err := b.Client.WorkflowService().GetWorkflowExecutionHistory(ctx, &workflowservice.GetWorkflowExecutionHistoryRequest{
		Namespace: b.Namespace, Execution: &commonpb.WorkflowExecution{WorkflowId: in.RunID}, MaximumPageSize: 1,
		HistoryEventFilterType: enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	})
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Temporal pages contain persistence batches: MaximumPageSize=1 can return
	// the start event and later events from that same batch. Inspect only event1,
	// while independently bounding the complete returned page.
	if response == nil || len(response.RawHistory) > 0 || len(response.GetHistory().GetEvents()) == 0 || len(response.GetHistory().GetEvents()) > 4096 || proto.Size(response) > 4<<20 {
		return false, errors.New("direct engine: bounded start history unavailable")
	}
	event := response.History.Events[0]
	started := event.GetWorkflowExecutionStartedEventAttributes()
	if event.GetEventId() != 1 || started == nil || started.GetTaskQueue().GetName() != b.TaskQueue || started.GetWorkflowType().GetName() != "Run" {
		return false, errors.New("direct engine: observed workflow start or task queue differs")
	}
	if err = validatePayloadSize(started.Input); err != nil {
		return false, err
	}
	// Decode the complete JSON value, rather than projecting into RunInput:
	// dropping unknown fields could falsely match input interpreted by a newer
	// worker. The SDK writes the same canonical encoding/json bytes at start.
	var actual json.RawMessage
	if err = b.Converter.FromPayloads(started.Input, &actual); err != nil {
		return false, fmt.Errorf("direct engine: decode accepted history: %w", err)
	}
	if len(actual) > triggerqueue.MaxDirectEngineInputBytes || Digest(actual) != digest {
		return false, errors.New("direct engine: observed input differs from accepted snapshot")
	}
	if marker := started.GetMemo().GetFields()[engine.RunInputDigestMemoKey]; marker != nil {
		var value string
		if len(marker.Data) > triggerqueue.MaxPayloadBytes {
			return false, errors.New("direct engine: oversized acceptance memo")
		}
		if err = b.Converter.FromPayload(marker, &value); err != nil {
			return false, err
		}
		if value != digest {
			return false, errors.New("direct engine: observed acceptance memo differs")
		}
	}
	return true, nil
}

func validatePayloadSize(input *commonpb.Payloads) error {
	if input == nil || len(input.Payloads) != 1 {
		return errors.New("direct engine: unexpected history input count")
	}
	// Allow the existing sealed codec's ciphertext envelope overhead. The
	// decoded canonical snapshot still has its exact bounded accepted digest.
	if len(input.Payloads[0].Data) > 2*triggerqueue.MaxDirectEngineInputBytes {
		return errors.New("direct engine: oversized history input")
	}
	return nil
}
