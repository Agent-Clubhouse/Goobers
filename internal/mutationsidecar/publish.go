package mutationsidecar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// ReceiptEmitter acknowledges durable host journal publication.
type ReceiptEmitter interface {
	Emit(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error)
}

// PublishBeforeCleanup preserves every receipt before a worker destroys its
// source. The caller supplies trusted run ownership and a bounded context.
// Missing sidecars need no emitter; malformed or uncertain handoffs fail closed.
// The source is never modified, including after a partial or lost ACK.
func PublishBeforeCleanup(ctx context.Context, workspace, worktreeID, runID, gaggle string, scrubber journal.Scrubber, emitter ReceiptEmitter) error {
	data, err := Read(workspace)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	facts, err := ParseRecoveryFacts(data)
	if err != nil || len(facts) == 0 {
		return err
	}
	if !apiv1.ValidRunID(runID) || worktreeID == "" || emitter == nil || scrubber == nil {
		return fmt.Errorf("mutation receipt handoff requires trusted ownership, scrubber, and host emitter")
	}
	events, err := missingRecoveryEvents(facts, nil, worktreeID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		// Changing signed/attributable receipt fields during transport would
		// make the acknowledgment cover different evidence. Preserve locally
		// for reconciliation instead of transmitting any detected credential.
		if len(encoded) > 1<<20 || !bytes.Equal(encoded, scrubber.Scrub(encoded)) {
			return fmt.Errorf("mutation receipt is oversized or requires credential reconciliation")
		}
		key := fmt.Sprintf("mutation-recovery/%x", sha256.Sum256(encoded))
		response, err := emitter.Emit(ctx, livejournal.EmitRequest{RunID: runID, Gaggle: gaggle, Ops: []livejournal.Op{{
			Kind: livejournal.OpAppend, Key: key, Event: &event, Time: time.Now().UTC(),
		}}})
		if err != nil {
			return err
		}
		if response.Applied < 0 || response.Deduplicated < 0 || response.Applied+response.Deduplicated != 1 || response.Seq == 0 {
			return fmt.Errorf("host did not acknowledge the mutation receipt")
		}
	}
	return nil
}
