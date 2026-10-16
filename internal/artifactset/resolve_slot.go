package artifactset

import (
	"context"

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
	binding, err := LookupSlot(ctx, reader, pointers, producer, slot)
	if err != nil {
		return SlotPayload{}, err
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
