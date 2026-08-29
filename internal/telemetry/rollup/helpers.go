package rollup

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

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

// maxStoredMessageLen bounds every free-text error message column ingest
// writes (run_errors.message, scheduler_errors.message). Without a cap, one
// pathological event grows a rollup row without bound: a Jul 21-22 incident's
// errors.Join of ~1,715 per-directory failures produced a single 2.6MB
// message, and 108 such rows totaled 281MB — 33% of telemetry.db.
const maxStoredMessageLen = 64 * 1024

// capMessage bounds message to maxStoredMessageLen bytes. Call it AFTER
// redaction (telemetry.Redact) so a secret-shaped substring can never be left
// half-exposed by a cut landing inside it. Anything past the limit is
// replaced by a marker carrying the untruncated (already-redacted) size and a
// SHA-256 prefix of the full text, so a truncated row can still be correlated
// against the untruncated line in its source journal during forensics — the
// journal itself is never capped, only what this rollup stores.
func capMessage(message string) string {
	if len(message) <= maxStoredMessageLen {
		return message
	}
	sum := sha256.Sum256([]byte(message))
	marker := fmt.Sprintf("...[truncated %d bytes, sha256:%x]", len(message), sum[:8])
	keep := maxStoredMessageLen - len(marker)
	if keep < 0 {
		keep = 0
	}
	// Never split a multi-byte UTF-8 rune at the cut point.
	for keep > 0 && !utf8.RuneStart(message[keep]) {
		keep--
	}
	return message[:keep] + marker
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
