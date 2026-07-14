package rollup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullIfZeroInt(n int) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

func nullIfZeroInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}

// timeFormat is RFC3339 with a fixed-width 9-digit fractional second — unlike
// time.RFC3339Nano's ".999999999" (which trims trailing zeros: "12:00:00Z"
// and "12:00:00.5Z" and "12:00:00.500000000Z" are three different string
// lengths for what could be three same-second events), ".000000000" always
// pads to the full width. Lexicographic string ORDER BY / range comparisons
// (aggregates.go's time-window filters, query.go's ORDER BY occurred_at) only
// agree with chronological order when every row's timestamp string is the
// same width — issue #129's checklist. Parsing is unaffected: time.Parse
// accepts any fractional-second width regardless of which layout formatted
// it, so parseTime (time.RFC3339Nano) reads both old (trimmed) and new
// (fixed-width) rows the same way — no migration needed for existing rows.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders a timestamp as fixed-width RFC3339Nano UTC text.
// Timestamps are always bound as explicit strings (never left to a driver's
// implicit time.Time conversion) so rollup rows are byte-for-byte
// reproducible across drivers and across an ingest/rebuild cycle (the
// rebuild-is-byte-identical acceptance criterion, #22).
func formatTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(timeFormat), Valid: true}
}

func durationMillis(start, end time.Time) sql.NullInt64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: end.Sub(start).Milliseconds(), Valid: true}
}

func runnerJSON(m map[string]any) (sql.NullString, error) {
	if len(m) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("rollup: marshal runner annotations: %w", err)
	}
	// Redact over the raw JSON text: a secret-shaped substring inside a JSON
	// string value still matches (quoting doesn't hide it from the pattern
	// net), so this is a correct, simpler alternative to walking values.
	return sql.NullString{String: telemetry.Redact(string(b)), Valid: true}, nil
}

func marshalAttributes(attrs map[string]string) (sql.NullString, error) {
	if len(attrs) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("rollup: marshal span event attributes: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// operationFromRunner reads a string "operation" annotation from the journal
// event's runner.* namespace, if a runner chose to stash one there. The v1
// journal event schema (internal/journal, #8) does not carry a dedicated
// mutation-operation field on ref.touched — providers.ExternalRef (#12) does,
// via its Operation field — so until the runner's #8-wiring settles on a home
// for it, this reads it from the one sanctioned runner-specific escape hatch.
// Absent entirely, provider_mutations.operation is simply NULL.
func operationFromRunner(m map[string]any) string {
	if m == nil {
		return ""
	}
	op, _ := m["operation"].(string)
	return op
}
