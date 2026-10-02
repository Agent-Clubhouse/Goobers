package main

func truncateRunes(value string, max int, suffix string, suffixCountsTowardLimit bool) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if !suffixCountsTowardLimit {
		return string(runes[:max]) + suffix
	}
	suffixRunes := []rune(suffix)
	if max <= len(suffixRunes) {
		return string(runes[:max])
	}
	return string(runes[:max-len(suffixRunes)]) + suffix
}
