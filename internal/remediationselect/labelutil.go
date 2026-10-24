package remediationselect

import "strings"

// HasAnyLabel reports whether labels contains any of wants (case-sensitive,
// matching GitHub's own label-name comparison).
func HasAnyLabel(labels, wants []string) bool {
	for _, w := range wants {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		for _, l := range labels {
			if l == w {
				return true
			}
		}
	}
	return false
}
