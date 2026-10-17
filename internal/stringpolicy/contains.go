package stringpolicy

import "strings"

// ContainsAny reports whether value contains at least one of the substrings.
// Matching is case-sensitive. An empty list returns false; an empty substring
// matches every value, including an empty value.
func ContainsAny(value string, substrings ...string) bool {
	for _, substring := range substrings {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
