//go:build !unix

package proc

import "context"

func probeCleanup(context.Context) CleanupProbe {
	return CleanupProbe{"cleanup_guarantee_unavailable", "unsupported", "controlled descendant fixture is unavailable on this platform; no claim about target cleanup"}
}
