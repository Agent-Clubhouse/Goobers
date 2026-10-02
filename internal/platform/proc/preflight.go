package proc

import "context"

// CleanupProbe is evidence about a disposable local fixture, never a live
// daemon, worker, escaped/reparented remote process, or a provider resource.
type CleanupProbe struct {
	Code    string `json:"code"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// ProbeCleanup force-stops a controlled parent and descendant through Start
// and Tree.Kill. It starts no workload and performs no provider operations.
func ProbeCleanup(ctx context.Context) CleanupProbe {
	if ctx.Err() != nil {
		return CleanupProbe{"cleanup_probe_canceled", "unobservable", "fixture canceled before launch"}
	}
	return probeCleanup(ctx)
}
