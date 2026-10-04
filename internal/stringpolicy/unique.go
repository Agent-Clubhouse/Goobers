// Package stringpolicy contains shared string collection policies.
package stringpolicy

// Unique returns the exact values in first-seen order with duplicates removed.
// It does not trim or discard empty values.
func Unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// AppendUnique appends value unless values already contains it.
func AppendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
