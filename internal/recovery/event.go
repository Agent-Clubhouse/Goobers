package recovery

import (
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// RetainedEvent describes successfully published recovery state without
// disclosing archive contents or local credential/workspace paths. The caller
// must durably publish the archive and record first; validation alone does not
// establish artifact durability. Append this to the instance log, not by
// reopening the active run's exclusively held writer.
func RetainedEvent(record Record) (journal.Event, error) {
	if err := record.Validate(); err != nil {
		return journal.Event{}, err
	}
	event := journal.Event{
		RunID: record.RunID,
		Type:  journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"operation":             "recovery-retained",
			"recoveryRepositoryKey": record.RepositoryKey,
			"recoveryRef":           record.Ref,
			"recoveryBaseRef":       record.BaseRef,
			"recoveryBaseSHA":       record.BaseSHA,
			"recoverySnapshotSHA":   record.SnapshotSHA,
			"recoveryPatchDigest":   record.PatchDigest,
			"recoveryArchiveDigest": record.ArchiveDigest,
			"recoveryArchiveBytes":  record.ArchiveBytes,
			"recoveryArchiveFormat": record.ArchiveFormat,
			"recoveryRetainUntil":   record.RetainUntil.UTC().Format(time.RFC3339Nano),
			"recoveryCreatedAt":     record.CreatedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if record.BaseRef == "" {
		delete(event.Runner, "recoveryBaseRef")
	}
	if record.ArchiveFormat == "" {
		delete(event.Runner, "recoveryArchiveFormat")
	}
	return event, nil
}
