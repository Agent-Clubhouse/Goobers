package recovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// RecordsFromEvents reads host-written observations from one run's journal.
// Stage-originated emissions carry emitKey and cannot assert archive custody.
// A later observation may extend, but never shorten or replace, the same ref.
func RecordsFromEvents(events []journal.Event, runID string) ([]Record, error) {
	records := make(map[string]Record)
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["operation"] != "recovery-retained" {
			continue
		}
		if _, emitted := event.Runner[livejournal.EmitKeyRunnerField]; emitted {
			continue
		}
		if event.RunID != runID {
			return nil, fmt.Errorf("recovery observation belongs to another run")
		}
		record, err := recordFromEvent(event)
		if err != nil {
			return nil, err
		}
		key := record.RepositoryKey + "\x00" + record.Ref
		if prior, ok := records[key]; ok {
			comparison := record
			comparison.RetainUntil = prior.RetainUntil
			if comparison != prior {
				return nil, ErrRecordConflict
			}
			if !record.RetainUntil.After(prior.RetainUntil) {
				continue
			}
		}
		records[key] = record
		if len(records) > 128 {
			return nil, fmt.Errorf("recovery observations exceed inventory bound")
		}
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]Record, 0, len(keys))
	for _, key := range keys {
		result = append(result, records[key])
	}
	return result, nil
}

func recordFromEvent(event journal.Event) (Record, error) {
	fields := map[string]any{"version": 1, "runId": event.RunID, "archiveBytes": event.Runner["recoveryArchiveBytes"]}
	if value, exists := event.Runner["recoveryBaseRef"]; exists {
		baseRef, ok := value.(string)
		if !ok || len(baseRef) > 4096 {
			return Record{}, fmt.Errorf("invalid recovery observation field recoveryBaseRef")
		}
		fields["baseRef"] = baseRef
	}
	if value, exists := event.Runner["recoveryArchiveFormat"]; exists {
		archiveFormat, ok := value.(string)
		if !ok || len(archiveFormat) > 4096 {
			return Record{}, fmt.Errorf("invalid recovery observation field recoveryArchiveFormat")
		}
		fields["archiveFormat"] = archiveFormat
	}
	for field, source := range map[string]string{
		"repositoryKey": "recoveryRepositoryKey", "ref": "recoveryRef",
		"baseSha": "recoveryBaseSHA", "snapshotSha": "recoverySnapshotSHA",
		"patchDigest": "recoveryPatchDigest", "archiveDigest": "recoveryArchiveDigest",
		"createdAt": "recoveryCreatedAt", "retainUntil": "recoveryRetainUntil",
	} {
		value, ok := event.Runner[source].(string)
		if !ok || len(value) > 4096 {
			return Record{}, fmt.Errorf("invalid recovery observation field %s", source)
		}
		fields[field] = value
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return Record{}, err
	}
	return Decode(bytes.NewReader(data))
}
