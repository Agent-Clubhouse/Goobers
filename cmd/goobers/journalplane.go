package main

import (
	"slices"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel/intake"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
)

const engineTopologyRestartMessage = "adding or removing a gaggle requires a daemon restart while distributed journal writing is enabled"

// engineTopologyChanged reports whether next's gaggle set differs from
// current's (#3642). buildLiveJournalWriter's runsDirs are computed once from
// configuredGaggleNames(set) at boot and never rebuilt on config reload, so a
// gaggle added or removed after boot would otherwise run with no journal
// support until a restart. A reload that changes the visible gaggle set is rejected the
// same way webhookListenerTopologyChanged already rejects a webhook
// listener topology change. A nil/disabled engine has no such boot-pinned
// consumer, so only matters while EngineProjectionEnabled.
func engineTopologyChanged(current, next *instance.ConfigSet, cfg *instance.Config) bool {
	if !cfg.EngineProjectionEnabled() {
		return false
	}
	return !slices.Equal(configuredGaggleNames(current), configuredGaggleNames(next))
}

// journalplane.go wires the daemon side of the write API's journal plane
// (distributed-state-and-coordination.md §7/§8, DS4): the live journal writer
// that authors distributed journals from emitted events and its HTTP service.

// newLiveJournalWriter is buildLiveJournalWriter behind a var so up.go's
// construction remains assertable at the call site.
var newLiveJournalWriter = buildLiveJournalWriter

// buildLiveJournalWriter builds the daemon's stage-pod journal writer when
// distributed execution is configured. Returns nil for a local-only instance.
//
// blobs is the span source (#3805): the daemon's own blob store, which is
// also what a mode-3 stage pod PUTs its scrubbed transcript into over the
// blob plane before the workflow emits the pointer-only span op. Without it
// every pod-executed agentic stage records an error.code=span_unavailable
// event in place of its span.recorded — measured on five live runs. nil is
// accepted and keeps the old degrade-softly behaviour.
func buildLiveJournalWriter(l instance.Layout, cfg *instance.Config, set *instance.ConfigSet, watermarks *intake.Store, instanceLog *journal.InstanceLog, blobs blobstore.Store, providerQuota *localscheduler.ProviderQuotaState) (*livejournal.Writer, error) {
	if cfg == nil || !cfg.EngineProjectionEnabled() {
		return nil, nil
	}
	runsDirs := make(map[string]string)
	for _, gaggle := range configuredGaggleNames(set) {
		runsDirs[gaggle] = l.ForGaggle(gaggle).RunsDir()
	}
	opts := []livejournal.Option{}
	if blobs != nil {
		opts = append(opts, livejournal.WithSpanSource(blobs))
		// Prepared artifact sets use content references instead of placing
		// large payloads in journal HTTP requests. Adoption must remain
		// bounded; a store without this capability cannot accept reference ops.
		if bounded, ok := blobs.(livejournal.ArtifactSource); ok {
			opts = append(opts, livejournal.WithArtifactSource(bounded))
		}
	}
	if observer := telemetryingest.RunObserversContext(watermarks, l, instanceLog); observer != nil {
		// The same read-model intake the local runner notifies per append —
		// which is what makes a live engine run's stage transitions reach SSE
		// and the portal through the existing machinery, mid-run. It also
		// queues Backprop stage checkpoints for stage-granular workflows.
		opts = append(opts, livejournal.WithContextObserver(observer))
	}
	if instanceLog != nil {
		// The destination of a pod's OpInstanceAnnotation (Goobers#3898): the
		// scheduler's cross-run narration belongs in THIS file, on the
		// daemon's volume, and a stage pod has no legal path to it. The pod
		// emits over the run-scoped journal plane and the daemon appends here,
		// with the event's RunID restamped from the request so the run
		// containment the route enforces cannot be sidestepped by what the
		// caller put in the payload.
		opts = append(opts, livejournal.WithInstanceLog(instanceLog))
	}
	return livejournal.NewWriter(func(gaggle string) (string, bool) {
		dir, ok := runsDirs[gaggle]
		return dir, ok
	}, opts...)
}
