// Package boundedagg aggregates an unbounded collection of errors or strings
// into a single record whose item count AND byte size are both capped. It
// exists because a sweep over a pathological number of bad entries — the ~10k
// orphan run directories behind the 281 MB scheduler-journal bloat (#1166,
// #1414) — must never build a multi-megabyte message that is then persisted and
// re-ingested. The helpers here bound the record at the SOURCE (the aggregation
// site) and at the write boundary (Bound), appending a truncation marker so a
// clipped record is never mistaken for the whole value.
package boundedagg

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxItems caps how many individual entries an aggregated record
	// retains before the remainder is collapsed into a single "…and N more"
	// count. Chosen so a real burst of distinct failures is still legible
	// while a pathological sweep cannot balloon the item list.
	DefaultMaxItems = 100
	// DefaultMaxBytes caps the total byte size of an aggregated or bounded
	// record. The full detail already lives in each run's own journal; this is
	// purely a cap on the coarse echo persisted to the scheduler journal.
	DefaultMaxBytes = 16 << 10 // 16 KiB

	truncationSuffix = "…(truncated)"
)

// Join is a size-bounded replacement for errors.Join for accumulating the
// per-entry errors of a sweep over an unbounded collection. Like errors.Join
// it returns nil when every argument is nil; otherwise it returns a single
// error whose message is the newline-joined per-entry messages, capped at
// DefaultMaxItems entries and DefaultMaxBytes with a "…and N more (truncated)"
// marker. Unlike errors.Join the result does not support errors.Is/As on the
// wrapped entries — it is a flattened, persistable message by design.
func Join(errs ...error) error {
	return JoinLimit(DefaultMaxItems, DefaultMaxBytes, errs...)
}

// JoinLimit is Join with explicit caps. A non-positive maxItems or maxBytes
// disables that particular cap.
func JoinLimit(maxItems, maxBytes int, errs ...error) error {
	msgs := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(Strings(msgs, maxItems, maxBytes))
}

// Strings joins items with newlines, keeping at most maxItems entries and at
// most maxBytes of joined content, then appends a "…and N more (truncated)"
// marker reporting how many entries were dropped. A non-positive maxItems or
// maxBytes disables that particular cap. The returned string is bounded to
// roughly maxBytes plus the short marker.
func Strings(items []string, maxItems, maxBytes int) string {
	var b strings.Builder
	kept := 0
	for _, item := range items {
		if maxItems > 0 && kept >= maxItems {
			break
		}
		sep := 0
		if kept > 0 {
			sep = len("\n")
		}
		if maxBytes > 0 && kept > 0 && b.Len()+sep+len(item) > maxBytes {
			break
		}
		if kept > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(item)
		kept++
	}
	out := b.String()
	// Backstop for a single oversized first entry, which the per-item byte
	// check above deliberately keeps whole: hard-cap the body itself.
	if maxBytes > 0 && len(out) > maxBytes {
		out = truncateBytes(out, maxBytes)
	}
	if omitted := len(items) - kept; omitted > 0 {
		if out != "" {
			out += "\n"
		}
		out += fmt.Sprintf("…and %d more (truncated)", omitted)
	}
	return out
}

// Bound caps a single, already-composed message at maxBytes, appending a
// truncation marker so a clipped record is never mistaken for the whole value.
// It is the write-boundary guard applied just before a diagnostic string is
// persisted, catching giant messages built anywhere upstream. A non-positive
// maxBytes leaves the message unchanged.
func Bound(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return truncateBytes(s, maxBytes)
}

// truncateBytes clips s to at most maxBytes (including the suffix), trimming to
// a UTF-8 rune boundary so a multi-byte rune is never split, and appends the
// truncation marker.
func truncateBytes(s string, maxBytes int) string {
	limit := maxBytes - len(truncationSuffix)
	if limit < 0 {
		limit = 0
	}
	if limit > len(s) {
		limit = len(s)
	}
	for limit > 0 && limit < len(s) && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + truncationSuffix
}
