package artifactset

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Record stores sanitized bytes in the current run journal and returns the
// runner-authored pointer. name is a semantic recording name, not a disk path.
type Record func(name, mediaType string, data []byte) (apiv1.ArtifactPointer, error)

// Publish returns index-first pointers only after the complete prepared set has
// been durably recorded. Failed writes may leave unreferenced journal blobs for
// ordinary retention, but never return a partially usable result artifact set.
func (p *Prepared) Publish(ctx context.Context, record Record) ([]apiv1.ArtifactPointer, error) {
	if p == nil || record == nil {
		return nil, fmt.Errorf("%w: missing prepared set or recorder", ErrInvalid)
	}
	index := Index{SchemaVersion: SchemaVersion, Entries: make([]Entry, 0, len(p.entries))}
	pointers := make([]apiv1.ArtifactPointer, len(p.entries)+1)
	for i, entry := range p.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pointer, err := recordExact(record, entry.name, entry.mediaType, entry.data)
		if err != nil {
			return nil, err
		}
		pointers[i+1] = pointer
		index.Entries = append(index.Entries, Entry{Name: entry.name, Slot: i + 1, Artifact: pointer})
	}
	data, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxIndexBytes {
		return nil, fmt.Errorf("%w: normalized index exceeds byte limit", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pointers[0], err = recordExact(record, "artifact-set.json", "application/json", data)
	if err != nil {
		return nil, err
	}
	return pointers, nil
}

func recordExact(record Record, name, mediaType string, data []byte) (apiv1.ArtifactPointer, error) {
	pointer, err := record(name, mediaType, data)
	if err != nil {
		return apiv1.ArtifactPointer{}, err
	}
	if err := pointer.Validate(); err != nil {
		return apiv1.ArtifactPointer{}, err
	}
	if pointer.MediaType != mediaType || pointer.Size != int64(len(data)) || pointer.Digest != apiv1.Digest(data) {
		return apiv1.ArtifactPointer{}, fmt.Errorf("%w: recorder changed prepared artifact", ErrInvalid)
	}
	return pointer, nil
}
