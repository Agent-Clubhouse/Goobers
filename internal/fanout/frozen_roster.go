package fanout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// FrozenRosterSchemaVersion identifies the content-addressed roster blob.
const FrozenRosterSchemaVersion = "goobers.dev/frozen-map-roster/v1"

// MaxFrozenRosterBytes bounds the normalized content-addressed roster shared
// by the runner and engine. It is separate from the producer source bound.
const MaxFrozenRosterBytes = MaxSourceBytes

// FrozenRosterBlob is the normalized, content-addressed representation of one
// map visit. The map definition is pinned separately in the workflow input;
// Roster records the exact source attempt, effective bounds, and canonical
// item payloads needed to verify that definition on replay.
type FrozenRosterBlob struct {
	SchemaVersion string            `json:"schemaVersion"`
	Roster        journal.MapRoster `json:"roster"`
}

// FreezeRosterBlob verifies a journal-bound source, freezes its canonical
// roster, and returns both the decoded contract and its stable JSON bytes.
func FreezeRosterBlob(def apiv1.Map, source journal.FanOutSourceRef, sourceData []byte) (FrozenRosterBlob, []byte, string, error) {
	if err := validateFrozenSource(def, source, sourceData); err != nil {
		return FrozenRosterBlob{}, nil, "", err
	}
	items, err := FreezeRoster(sourceData, source.MediaType)
	if err != nil {
		return FrozenRosterBlob{}, nil, "", err
	}
	frozenSource := source
	frozenSource.Visit, frozenSource.Path = 0, ""
	blob := FrozenRosterBlob{
		SchemaVersion: FrozenRosterSchemaVersion,
		Roster: journal.MapRoster{
			Source:             frozenSource,
			FailurePolicy:      def.FailurePolicy,
			ItemTimeoutSeconds: def.ItemTimeoutSeconds,
			MaxConcurrent:      EffectiveMaxConcurrent(def),
			Items:              items,
		},
	}
	if _, err := NewMapVisit(journal.Event{
		Seq: 1, Type: journal.EventMapStarted, Parallel: def.Name, MapRoster: &blob.Roster,
	}, def); err != nil {
		return FrozenRosterBlob{}, nil, "", err
	}
	data, err := encodeFrozenRosterBlob(blob)
	if err != nil {
		return FrozenRosterBlob{}, nil, "", fmt.Errorf("encode frozen map roster: %w", err)
	}
	if int64(len(data)) > MaxFrozenRosterBytes {
		return FrozenRosterBlob{}, nil, "", &SourceError{
			Reason: ReasonTooLarge,
			Detail: fmt.Sprintf("normalized roster is %d bytes, bound is %d", len(data), MaxFrozenRosterBytes),
		}
	}
	return blob, data, journal.Digest(data), nil
}

// DecodeRosterBlob verifies the blob digest, canonical encoding, and complete
// roster contract against the pinned map definition.
func DecodeRosterBlob(data []byte, digest string, def apiv1.Map) (FrozenRosterBlob, error) {
	if !ValidDigest(digest) || journal.Digest(data) != digest {
		return FrozenRosterBlob{}, fmt.Errorf("frozen map roster digest does not match its bytes")
	}
	if int64(len(data)) > MaxFrozenRosterBytes {
		return FrozenRosterBlob{}, &SourceError{
			Reason: ReasonTooLarge,
			Detail: fmt.Sprintf("normalized roster is %d bytes, bound is %d", len(data), MaxFrozenRosterBytes),
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var blob FrozenRosterBlob
	if err := decoder.Decode(&blob); err != nil {
		return FrozenRosterBlob{}, fmt.Errorf("decode frozen map roster: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return FrozenRosterBlob{}, fmt.Errorf("decode frozen map roster: trailing JSON value")
	}
	if blob.SchemaVersion != FrozenRosterSchemaVersion {
		return FrozenRosterBlob{}, fmt.Errorf("frozen map roster schemaVersion %q, want %q", blob.SchemaVersion, FrozenRosterSchemaVersion)
	}
	if _, err := NewMapVisit(journal.Event{
		Seq: 1, Type: journal.EventMapStarted, Parallel: def.Name, MapRoster: &blob.Roster,
	}, def); err != nil {
		return FrozenRosterBlob{}, err
	}
	canonical, err := encodeFrozenRosterBlob(blob)
	if err != nil {
		return FrozenRosterBlob{}, fmt.Errorf("re-encode frozen map roster: %w", err)
	}
	if !bytes.Equal(data, canonical) {
		return FrozenRosterBlob{}, fmt.Errorf("frozen map roster is not canonically encoded")
	}
	return blob, nil
}

func encodeFrozenRosterBlob(blob FrozenRosterBlob) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(blob); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func validateFrozenSource(def apiv1.Map, source journal.FanOutSourceRef, data []byte) error {
	switch {
	case def.Name == "":
		return fmt.Errorf("map name must not be empty")
	case source.Task != def.Source.Task || source.Slot != def.Source.Slot:
		return fmt.Errorf("source %s.%s differs from pinned map source %s.%s",
			source.Task, source.Slot, def.Source.Task, def.Source.Slot)
	case source.Visit == 0 || source.Attempt < 1:
		return fmt.Errorf("source must identify a settled producer visit and attempt")
	case !ValidDigest(source.Digest):
		return fmt.Errorf("source digest %q is malformed", source.Digest)
	case source.MediaType != SourceMediaType:
		return &SourceError{Reason: ReasonMediaType, Detail: fmt.Sprintf("media type %q, want %q", source.MediaType, SourceMediaType)}
	case source.Size < 0 || source.Size > MaxSourceBytes:
		return &SourceError{Reason: ReasonTooLarge, Detail: fmt.Sprintf("%d bytes exceeds the %d byte bound", source.Size, MaxSourceBytes)}
	case source.Size != int64(len(data)):
		return fmt.Errorf("source size %d does not match %d stored bytes", source.Size, len(data))
	case journal.Digest(data) != source.Digest:
		return fmt.Errorf("source digest %s does not match stored bytes", source.Digest)
	case !source.Integrity.Valid():
		return fmt.Errorf("source integrity %q is invalid", source.Integrity)
	}
	return nil
}
