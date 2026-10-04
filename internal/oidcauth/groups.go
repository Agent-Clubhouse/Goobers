package oidcauth

import (
	"slices"
	"strings"
	"unicode"
)

// Missing, malformed or oversized group data grants no group authority. It
// never grants a partially decoded membership or changes existing instance
// role authentication for monitoring-only users.
func verifiedGroups(raw any) []string {
	if raw == nil {
		return nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) > 128 {
		return nil
	}
	groups := make([]string, 0, len(values))
	for _, raw := range values {
		group, ok := raw.(string)
		if !ok || group == "" || len(group) > 512 || strings.TrimSpace(group) != group || strings.IndexFunc(group, unicode.IsControl) >= 0 {
			return nil
		}
		groups = append(groups, group)
	}
	slices.Sort(groups)
	return slices.Compact(groups)
}
