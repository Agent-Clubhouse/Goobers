package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name                    string
		value                   string
		max                     int
		suffix                  string
		suffixCountsTowardLimit bool
		want                    string
	}{
		{name: "ASCII unchanged below width", value: "abc", max: 4, suffix: "...", suffixCountsTowardLimit: true, want: "abc"},
		{name: "ASCII exact width", value: "abc", max: 3, suffix: "...", suffixCountsTowardLimit: true, want: "abc"},
		{name: "counting width zero", value: "abcdef", max: 0, suffix: "...", suffixCountsTowardLimit: true, want: ""},
		{name: "counting width one", value: "abcdef", max: 1, suffix: "...", suffixCountsTowardLimit: true, want: "a"},
		{name: "counting width two", value: "abcdef", max: 2, suffix: "...", suffixCountsTowardLimit: true, want: "ab"},
		{name: "counting width three", value: "abcdef", max: 3, suffix: "...", suffixCountsTowardLimit: true, want: "abc"},
		{name: "counting includes suffix", value: "abcdef", max: 4, suffix: "...", suffixCountsTowardLimit: true, want: "a..."},
		{name: "counting multibyte value", value: "修正ログイン", max: 5, suffix: "...", suffixCountsTowardLimit: true, want: "修正..."},
		{name: "counting multibyte suffix", value: "abcdef", max: 3, suffix: "界", suffixCountsTowardLimit: true, want: "ab界"},
		{name: "appending ASCII", value: "abcdef", max: 3, suffix: "...", want: "abc..."},
		{name: "appending multibyte value", value: "修正ログイン", max: 2, suffix: "... [truncated]", want: "修正... [truncated]"},
		{name: "appending exact width", value: "修正", max: 2, suffix: "...", want: "修正"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateRunes(tt.value, tt.max, tt.suffix, tt.suffixCountsTowardLimit)
			if got != tt.want {
				t.Fatalf("truncateRunes(%q, %d, %q, %t) = %q, want %q",
					tt.value, tt.max, tt.suffix, tt.suffixCountsTowardLimit, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncateRunes(%q, %d, %q, %t) returned invalid UTF-8",
					tt.value, tt.max, tt.suffix, tt.suffixCountsTowardLimit)
			}
		})
	}
}

func TestTruncationWrappers(t *testing.T) {
	t.Run("status and timeline preserve width", func(t *testing.T) {
		wantByWidth := []string{"", "修", "修正", "修正ロ", "修...", "修正...", "修正ログイン"}
		for width, want := range wantByWidth {
			if got := truncateStatusCell("修正ログイン", width); got != want {
				t.Errorf("truncateStatusCell width %d = %q, want %q", width, got, want)
			}
			if got := truncateTimelineText("修正ログイン", width); got != want {
				t.Errorf("truncateTimelineText width %d = %q, want %q", width, got, want)
			}
		}
	})

	t.Run("verdict appends suffix after limit", func(t *testing.T) {
		if got, want := truncateHuman("修正ログイン", 2), "修正... [truncated]"; got != want {
			t.Fatalf("truncateHuman = %q, want %q", got, want)
		}
		if got, want := truncateHuman("修正", 2), "修正"; got != want {
			t.Fatalf("truncateHuman at exact width = %q, want %q", got, want)
		}
	})

	t.Run("notify collapses whitespace before truncating", func(t *testing.T) {
		if got, want := oneLine("  alpha \n\t beta  "), "alpha beta"; got != want {
			t.Fatalf("oneLine whitespace = %q, want %q", got, want)
		}
		value := strings.Repeat("界", 241)
		if got, want := oneLine(value), strings.Repeat("界", 240)+"..."; got != want {
			t.Fatalf("oneLine truncation = %q, want %q", got, want)
		}
	})
}
