package boundedagg

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJoinReturnsNilWhenAllNil(t *testing.T) {
	if err := Join(nil, nil, nil); err != nil {
		t.Fatalf("Join(all nil) = %v, want nil", err)
	}
	if err := Join(); err != nil {
		t.Fatalf("Join() = %v, want nil", err)
	}
}

func TestJoinDropsNilEntries(t *testing.T) {
	err := Join(nil, errors.New("first"), nil, errors.New("second"))
	if err == nil {
		t.Fatal("Join with two non-nil errors returned nil")
	}
	want := "first\nsecond"
	if err.Error() != want {
		t.Fatalf("Join message = %q, want %q", err.Error(), want)
	}
}

func TestStringsUnderLimitIsUnchanged(t *testing.T) {
	items := []string{"a", "b", "c"}
	got := Strings(items, 10, 1024)
	want := "a\nb\nc"
	if got != want {
		t.Fatalf("Strings under limit = %q, want %q", got, want)
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("Strings under limit added a truncation marker: %q", got)
	}
}

func TestStringsOverCountKeepsFirstNAndCountsRest(t *testing.T) {
	items := make([]string, 10884)
	for i := range items {
		items[i] = fmt.Sprintf("err-%d", i)
	}
	got := Strings(items, 3, 0) // no byte cap; exercise the item cap alone
	kept := strings.Split(got, "\n")
	if len(kept) != 4 {
		t.Fatalf("kept lines = %d, want 3 entries + 1 marker: %q", len(kept), got)
	}
	if kept[0] != "err-0" || kept[2] != "err-2" {
		t.Fatalf("kept entries = %v, want first three", kept[:3])
	}
	if want := "…and 10881 more (truncated)"; kept[3] != want {
		t.Fatalf("marker = %q, want %q", kept[3], want)
	}
}

func TestStringsOverBytesStopsBeforeExceedingCap(t *testing.T) {
	items := make([]string, 1000)
	for i := range items {
		items[i] = strings.Repeat("x", 100) // 100 bytes each
	}
	const maxBytes = 512
	got := Strings(items, 0, maxBytes) // no item cap; exercise the byte cap alone
	body := got
	if idx := strings.LastIndex(got, "\n…and "); idx >= 0 {
		body = got[:idx]
	}
	if len(body) > maxBytes {
		t.Fatalf("body bytes = %d, want <= %d", len(body), maxBytes)
	}
	if !strings.Contains(got, "more (truncated)") {
		t.Fatalf("byte-capped result missing truncation marker: %q", got)
	}
	// The dropped count must be accurate: kept entries + omitted == total.
	keptEntries := strings.Count(body, "\n") + 1
	var omitted int
	if _, err := fmt.Sscanf(got[strings.LastIndex(got, "…and "):], "…and %d more (truncated)", &omitted); err != nil {
		t.Fatalf("parse omitted count: %v (got %q)", err, got)
	}
	if keptEntries+omitted != len(items) {
		t.Fatalf("kept %d + omitted %d != total %d", keptEntries, omitted, len(items))
	}
}

func TestStringsOversizedSingleEntryIsHardCapped(t *testing.T) {
	const maxBytes = 64
	got := Strings([]string{strings.Repeat("y", 10_000)}, 0, maxBytes)
	if len(got) > maxBytes {
		t.Fatalf("oversized single entry not hard-capped: %d bytes > %d", len(got), maxBytes)
	}
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatalf("hard-capped entry missing suffix: %q", got)
	}
}

func TestBoundLeavesShortStringUnchanged(t *testing.T) {
	s := "short message"
	if got := Bound(s, 1024); got != s {
		t.Fatalf("Bound short string = %q, want unchanged", got)
	}
	if got := Bound(s, 0); got != s {
		t.Fatalf("Bound with disabled cap = %q, want unchanged", got)
	}
}

func TestBoundTruncatesLongStringToByteCap(t *testing.T) {
	const maxBytes = 100
	got := Bound(strings.Repeat("z", 10_000), maxBytes)
	if len(got) > maxBytes {
		t.Fatalf("Bound result = %d bytes, want <= %d", len(got), maxBytes)
	}
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatalf("Bound result missing suffix: %q", got)
	}
}

func TestBoundDoesNotSplitMultiByteRune(t *testing.T) {
	// Every rune here is 3 bytes; a naive byte cut would leave invalid UTF-8.
	got := Bound(strings.Repeat("界", 100), 40)
	trimmed := strings.TrimSuffix(got, truncationSuffix)
	if !utf8.ValidString(trimmed) {
		t.Fatalf("Bound split a multi-byte rune: %q", got)
	}
}
