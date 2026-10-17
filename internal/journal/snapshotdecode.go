package journal

import (
	"context"
	"errors"
)

// ErrReadSnapshotLimit means event decoding stopped before retaining an excess record.
var ErrReadSnapshotLimit = errors.New("journal: read snapshot event limit exceeded")

// DecodeReadSnapshot interprets caller-bounded journal bytes using the same
// schema and committed-event rules as Reader. It never migrates or writes.
func DecodeReadSnapshot(ctx context.Context, manifest, identity, log []byte, maxEvents int) (RunIdentity, []EventRecord, error) {
	if maxEvents <= 0 {
		return RunIdentity{}, nil, ErrReadSnapshotLimit
	}
	if err := ctx.Err(); err != nil {
		return RunIdentity{}, nil, err
	}
	info, exists, err := decodeSchemaInfo(manifest)
	if err != nil {
		return RunIdentity{}, nil, err
	}
	current, err := admitJournalSchema("", info, exists)
	if err != nil {
		return RunIdentity{}, nil, err
	}
	if !current {
		return RunIdentity{}, nil, ErrJournalMigrationRequired
	}
	id, err := decodeIdentity(identity)
	if err != nil {
		return RunIdentity{}, nil, err
	}
	records, _, err := parseEventRecordsContext(ctx, log, maxEvents)
	if err != nil {
		return RunIdentity{}, nil, err
	}
	return id, records, ctx.Err()
}
