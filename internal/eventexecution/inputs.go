package eventexecution

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/goobers/goobers/internal/journal"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Inputs hydrates only an exact closed group, retaining original provenance for
// latest-suppressed members. Caller holds the accepted archive's shared lease.
func Inputs(ctx context.Context, queue *triggerqueue.Store, gaggle, groupID string) (triggerqueue.Record, eventing.ExecutionInputs, error) {
	record, start, err := queue.VerifiedEventStart(ctx, gaggle, groupID)
	if err != nil {
		return triggerqueue.Record{}, eventing.ExecutionInputs{}, err
	}
	inputs := eventing.ExecutionInputs{Manifest: eventing.InputManifest{Start: start, AcceptanceID: record.ID, Actor: record.Actor}, Envelopes: map[string][]byte{}}
	var after int64
	for {
		members, err := queue.EventMembers(ctx, gaggle, groupID, after, 100)
		if err != nil {
			return record, inputs, err
		}
		for _, member := range members {
			receipt, err := queue.EventInput(ctx, gaggle, groupID, member.ReceiptID)
			if err != nil {
				return record, inputs, err
			}
			inputs.Manifest.Members = append(inputs.Manifest.Members, eventing.InputMember{ReceiptID: receipt.ID, Sequence: receipt.Sequence, Digest: receipt.Digest, Producer: receipt.Producer, Selected: member.Selected})
			if member.Selected {
				inputs.Envelopes[receipt.ID] = receipt.Envelope
			}
			after = member.Sequence
		}
		if len(members) < 100 {
			break
		}
	}
	_, err = inputs.Validate(strings.TrimPrefix(record.ID, "trigger-"), gaggle)
	return record, inputs, err
}

func checkInputRedaction(inputs eventing.ExecutionInputs) error {
	_, scrubber := journal.DefaultScrubber()
	raw, err := inputs.Validate(strings.TrimPrefix(inputs.Manifest.AcceptanceID, "trigger-"), inputs.Manifest.Start.Gaggle)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, scrubber.Scrub(raw)) {
		return errors.New("eventexecution: manifest requires intake redaction")
	}
	for _, raw := range inputs.Envelopes {
		if !bytes.Equal(raw, scrubber.Scrub(raw)) {
			return errors.New("eventexecution: payload requires intake redaction")
		}
	}
	return nil
}
