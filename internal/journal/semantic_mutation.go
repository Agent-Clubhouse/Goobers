package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

// WithSemanticMutation preserves capture metadata without counting another
// external effect. It grants no skip/retry authority. Legacy events keep their
// exact shape when receipt is nil.
func WithSemanticMutation(event Event, receipt *mutationreceipt.Receipt) Event {
	if receipt == nil {
		return event
	}
	if event.Runner == nil {
		event.Runner = make(map[string]any)
	}
	event.Runner["semanticMutation"] = *receipt
	// These are capture metadata, not additional external touches. The existing
	// observation recorder still emits provider effects; counting both would
	// double-count success as soon as capture is enabled.
	event.Type = EventRunnerAnnotation
	event.ExternalRef = nil
	event.Runner["annotation"] = "provider-mutation-receipt"
	return event
}

// continuationMutationHistory freezes the source's currently durable evidence
// under its writer lock, including custody copies recovered after run.finished.
// Ancestor evidence is carried explicitly, never re-read from mutable sources.
// Unknown intents remain unknown; unrelated identical actions remain distinct.
func continuationMutationHistory(inherited []mutationreceipt.Receipt, events []Event) ([]mutationreceipt.Receipt, error) {
	receipts := make(map[string]mutationreceipt.Receipt)
	for _, receipt := range inherited {
		if err := mergeSemanticReceipt(receipts, receipt); err != nil {
			return nil, err
		}
	}
	for _, event := range events {
		value, ok := event.Runner["semanticMutation"]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode semantic mutation receipt: %w", err)
		}
		var receipt mutationreceipt.Receipt
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&receipt); err != nil {
			return nil, fmt.Errorf("decode semantic mutation receipt: %w", err)
		}
		if err := mergeSemanticReceipt(receipts, receipt); err != nil {
			return nil, err
		}
	}
	if len(receipts) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(receipts))
	for key := range receipts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]mutationreceipt.Receipt, 0, len(keys))
	for _, key := range keys {
		result = append(result, receipts[key])
	}
	return result, nil
}

func mergeSemanticReceipt(receipts map[string]mutationreceipt.Receipt, receipt mutationreceipt.Receipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	prior, ok := receipts[receipt.ID]
	if ok {
		if prior.RunID != receipt.RunID || prior.Mutation != receipt.Mutation {
			return fmt.Errorf("conflicting semantic mutation receipt identity")
		}
		if prior.Phase == "completed" {
			return nil
		}
	}
	receipts[receipt.ID] = receipt
	return nil
}

func continuationSourceIdentity(reader *Reader, events []Event) (RunIdentity, error) {
	identity, err := reader.Identity()
	if err != nil {
		return RunIdentity{}, err
	}
	identity.MutationHistory, err = continuationMutationHistory(identity.MutationHistory, events)
	if err != nil {
		return RunIdentity{}, fmt.Errorf("capture continuation mutation history: %w", err)
	}
	return identity, nil
}
