package decomposition

import "strings"

// Batch marker comment prefixes. The publisher owns writing these;
// select-source only needs to recognize one already present so
// several old escalation runs pointing at the same parent resolve to the
// existing batch instead of racing to create a second one (design doc §2.1).
// Forward-declared here, ahead of the publisher that emits them, exactly like
// backlog-query's own "goobers-claim"/"goobers-claim-release" marker
// convention this mirrors.
const (
	PreparedBatchMarkerPrefix  = "goobers-decomposition-prepared:"
	PublishedBatchMarkerPrefix = "goobers-decomposition-published:"
)

// HasExistingBatchMarker reports whether any comment already records a
// prepared or published decomposition batch for the parent it was posted to.
func HasExistingBatchMarker(commentBodies []string) bool {
	for _, body := range commentBodies {
		trimmed := strings.TrimSpace(body)
		if strings.HasPrefix(trimmed, PreparedBatchMarkerPrefix) || strings.HasPrefix(trimmed, PublishedBatchMarkerPrefix) {
			return true
		}
	}
	return false
}
