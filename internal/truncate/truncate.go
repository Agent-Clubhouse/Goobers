// Package truncate bounds strings by byte length without splitting runes.
package truncate

import "unicode/utf8"

// Bytes returns s unchanged if it fits in limit bytes; otherwise it cuts s at
// a rune boundary and appends suffix so the result is at most limit bytes.
// Invalid UTF-8 in s is cut at the nearest preceding rune start.
func Bytes(s string, limit int, suffix string) string {
	if len(s) <= limit {
		return s
	}
	keep := limit - len(suffix)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + suffix
}
