package artifactset

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SlotPayload is one named slot's digest-verified bytes together with the
// runner-authored binding that names the exact producer attempt behind them.
type SlotPayload struct {
	Binding SlotBinding
	Bytes   []byte
}

// ResolveSlot resolves one named DSL 3.1 slot from a producer's published set.
// It requires a named (v1alpha2) index, so the payload is bound to the
// producer's recorded stage, visit and attempt, never to an ordinal. The
// payload pointer's size is checked against maxBytes BEFORE any payload byte
// is read, and only the requested slot's payload is read.
func ResolveSlot(ctx context.Context, reader Reader, pointers []apiv1.ContextPointer, producer, slot string, maxBytes int64) (SlotPayload, error) {
	if !semanticName.MatchString(producer) || !slotName.MatchString(slot) || reader == nil {
		return SlotPayload{}, fmt.Errorf("%w: invalid producer, slot or missing reader", ErrInvalid)
	}
	indexPointer, err := positional(pointers, producer, 0)
	if err != nil {
		return SlotPayload{}, err
	}
	if indexPointer.MediaType != "application/json" {
		return SlotPayload{}, fmt.Errorf("%w: index must be application/json", ErrInvalid)
	}
	data, err := readVerified(ctx, reader, indexPointer, MaxIndexBytes)
	if err != nil {
		return SlotPayload{}, err
	}
	var index Index
	if err := decodeIndex(data, &index); err != nil {
		return SlotPayload{}, err
	}
	if err := validateIndex(index, pointers, producer); err != nil {
		return SlotPayload{}, err
	}
	if index.SchemaVersion != NamedSchemaVersion {
		return SlotPayload{}, fmt.Errorf("%w: producer published no named slots", ErrInvalid)
	}
	for _, binding := range index.Bindings {
		if binding.Slot != slot {
			continue
		}
		if err := boundedPointer(binding.Artifact, maxBytes); err != nil {
			return SlotPayload{}, err
		}
		payload, err := readVerified(ctx, reader, binding.Artifact, maxBytes)
		if err != nil {
			return SlotPayload{}, err
		}
		return SlotPayload{Binding: binding, Bytes: payload}, nil
	}
	return SlotPayload{}, &PublicationError{Code: MissingSlotCode, Slot: slot}
}
