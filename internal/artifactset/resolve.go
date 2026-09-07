// Package artifactset implements the runner-authored, positional artifact-set
// handoff shared by agentic stages and deterministic evidence consumers.
package artifactset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SchemaVersion and the hard read bounds define the artifact-set wire contract.
const (
	SchemaVersion   = "goobers.dev/stage-artifact-set/v1alpha1"
	MaxEntries      = 64
	MaxIndexBytes   = 128 << 10
	MaxPayloadBytes = 16 << 20
	MaxSetBytes     = 64 << 20
)

// ErrInvalid distinguishes invalid evidence (a failed gate) from a reader's
// infrastructure error (eligible for the gate's infrastructure retry policy).
var ErrInvalid = errors.New("invalid artifact set")

var semanticName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Index is authored by the runner, never accepted from a completion envelope.
type Index struct {
	SchemaVersion string  `json:"schemaVersion"`
	Entries       []Entry `json:"entries"`
}

// Entry binds a semantic name to its exact one-based positional pointer.
type Entry struct {
	Name     string                `json:"name"`
	Slot     int                   `json:"slot"`
	Artifact apiv1.ArtifactPointer `json:"artifact"`
}

// Reader is bound by the runner to the current run's journal. Implementations
// must enforce containment and maxBytes before allocating or fetching content.
// No workspace, cross-run selector, or general filesystem is exposed to checks.
type Reader interface {
	ReadArtifact(context.Context, apiv1.ArtifactPointer, int64) ([]byte, error)
}

// Payload contains only digest-verified bytes and their runner-authored pointer.
type Payload struct {
	Artifact apiv1.ArtifactPointer
	Bytes    []byte
}

// Resolve verifies the complete index and all payloads, not merely the requested
// names. On any failure it returns no partial lookup. required names may be empty.
func Resolve(ctx context.Context, reader Reader, pointers []apiv1.ContextPointer, producer string, required ...string) (map[string]Payload, error) {
	if !semanticName.MatchString(producer) || reader == nil {
		return nil, fmt.Errorf("%w: invalid producer or missing reader", ErrInvalid)
	}
	indexPointer, err := positional(pointers, producer, 0)
	if err != nil {
		return nil, err
	}
	if indexPointer.MediaType != "application/json" {
		return nil, fmt.Errorf("%w: index must be application/json", ErrInvalid)
	}
	data, err := readVerified(ctx, reader, indexPointer, MaxIndexBytes)
	if err != nil {
		return nil, err
	}
	var index Index
	if err := decodeIndex(data, &index); err != nil {
		return nil, err
	}
	if err := validateIndex(index, pointers, producer); err != nil {
		return nil, err
	}
	result := make(map[string]Payload, len(index.Entries))
	var total int64
	for _, entry := range index.Entries {
		data, err := readVerified(ctx, reader, entry.Artifact, MaxPayloadBytes)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > MaxSetBytes {
			return nil, fmt.Errorf("%w: set exceeds byte limit", ErrInvalid)
		}
		result[entry.Name] = Payload{Artifact: entry.Artifact, Bytes: data}
	}
	for _, name := range required {
		if _, ok := result[name]; !ok {
			return nil, fmt.Errorf("%w: required semantic name absent", ErrInvalid)
		}
	}
	return result, nil
}

func validateIndex(index Index, pointers []apiv1.ContextPointer, producer string) error {
	if index.SchemaVersion != SchemaVersion || index.Entries == nil || len(index.Entries) > MaxEntries {
		return fmt.Errorf("%w: schema or entry count", ErrInvalid)
	}
	previous := ""
	var total int64
	for i, entry := range index.Entries {
		if !semanticName.MatchString(entry.Name) || entry.Name <= previous || entry.Slot != i+1 {
			return fmt.Errorf("%w: names must be unique and sorted; slots must be contiguous", ErrInvalid)
		}
		previous = entry.Name
		pointer, err := positional(pointers, producer, entry.Slot)
		if err != nil {
			return err
		}
		if pointer != entry.Artifact {
			return fmt.Errorf("%w: positional pointer mismatch", ErrInvalid)
		}
		if err := boundedPointer(pointer, MaxPayloadBytes); err != nil {
			return err
		}
		total += pointer.Size
		if total > MaxSetBytes {
			return fmt.Errorf("%w: set exceeds byte limit", ErrInvalid)
		}
	}
	return nil
}

func positional(pointers []apiv1.ContextPointer, producer string, slot int) (apiv1.ArtifactPointer, error) {
	name := fmt.Sprintf("%s.artifact[%d]", producer, slot)
	var result *apiv1.ArtifactPointer
	for _, pointer := range pointers {
		if pointer.Name != name {
			continue
		}
		if result != nil || pointer.Artifact == nil || pointer.External != nil || pointer.RunID != "" || pointer.Branch != 0 || pointer.BranchName != "" {
			return apiv1.ArtifactPointer{}, fmt.Errorf("%w: ambiguous or non-current-run context pointer", ErrInvalid)
		}
		result = pointer.Artifact
	}
	if result == nil {
		return apiv1.ArtifactPointer{}, fmt.Errorf("%w: missing positional pointer", ErrInvalid)
	}
	return *result, nil
}

func boundedPointer(pointer apiv1.ArtifactPointer, limit int64) error {
	if err := pointer.Validate(); err != nil {
		return fmt.Errorf("%w: malformed pointer: %w", ErrInvalid, err)
	}
	if pointer.Size < 0 || pointer.Size > limit {
		return fmt.Errorf("%w: pointer exceeds byte limit", ErrInvalid)
	}
	return nil
}

func readVerified(ctx context.Context, reader Reader, pointer apiv1.ArtifactPointer, limit int64) ([]byte, error) {
	if err := boundedPointer(pointer, limit); err != nil {
		return nil, err
	}
	data, err := reader.ReadArtifact(ctx, pointer, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || int64(len(data)) != pointer.Size || apiv1.Digest(data) != pointer.Digest {
		return nil, fmt.Errorf("%w: payload size or digest mismatch", ErrInvalid)
	}
	return data, nil
}

func decodeIndex(data []byte, index *Index) error {
	return decodeDocument(data, index)
}

func decodeDocument(data []byte, target any) error {
	// A second member with the same name must not silently override the first,
	// including within embedded pointers. Bound nesting independently of bytes.
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return fmt.Errorf("%w: malformed index: %w", ErrInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: malformed index: %w", ErrInvalid, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return nil
}

func uniqueJSON(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("JSON nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid JSON member")
			}
			seen[name] = true
		}
		if err := uniqueJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
