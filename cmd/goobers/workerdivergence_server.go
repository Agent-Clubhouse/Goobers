package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/runtimewiring"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// Worker config-tree divergence detection (#4153).
//
// THE FAILURE THIS SURFACES. The daemon's config tree is owned by
// workflowSource and advances on a git push; the worker's is a pod-private
// emptyDir seeded once, at deploy time, by an init container untarring a
// ConfigMap. Nothing writes to it again. Every agentic gate is pinned to the
// daemon's goober digest and served from the worker's tree, so a goober-content
// edit made through the supported config-repo path takes every agentic gate in
// the instance out of service until somebody happens to run a deploy.
//
// The refusal an operator sees says the attempt "recovers once a config reload
// brings the pinned tree into force." On this deployment that reload can never
// happen: the reloader is correct and its input is frozen. Two merge-review
// runs failed this way within seconds of each other and neither ever
// recovered.
//
// WHAT THIS DOES AND DOES NOT FIX. It does not give the worker's tree a
// writer — that is the structural fix, and it is a live design choice between
// serving the tree over the daemon API and #3290's rendered-config mirror.
// What it does is convert a SILENT divergence into an alertable one: the
// worker asks the daemon which tree is in force and says so, loudly and
// repeatedly, when that is not the tree it holds. Until this, the only signal
// was a run failing at its gate, after admission, with a message telling the
// operator to wait for something that was not coming.
//
// WHY POLL RATHER THAN CHECK AT REFUSAL TIME. gate_pin_missing already fires
// at refusal time and is already loud. The gap is everything BEFORE that: an
// instance can sit diverged for hours with no run happening to need the moved
// goober, and the first symptom is a merge gate failing. A standing poll makes
// the condition observable while it is still cheap to fix.

// workerDivergenceCheckInterval is how often the worker asks the daemon which
// config tree is in force.
//
// Deliberately much slower than the 10s tree-reload poll: that poll reads a
// local directory, while this is a network call to the control plane, and the
// condition it detects persists for as long as it takes an operator to
// redeploy. One minute is far inside the window that matters (the recorded
// incidents ran 32 minutes and until a deploy) and costs the daemon one bounded
// GET per worker per minute.
func newWorkerDivergenceHandlerOption(log *journal.InstanceLog, cfg *instance.Config) (httpapi.HandlerOption, error) {
	recorder, err := newWorkerDivergenceJournalRecorder(log)
	if err != nil {
		return nil, fmt.Errorf("initialize journal: %w", err)
	}
	if err := recordDaemonWorkerDivergenceAvailability(recorder, cfg); err != nil {
		return nil, fmt.Errorf("record daemon availability: %w", err)
	}
	return httpapi.WithWorkerConfigDivergence(recorder.Append), nil
}

// divergenceReport is one comparison's outcome.
const (
	workerDivergenceInSync     = string(journal.WorkerConfigDivergenceInSync)
	workerDivergenceDiverged   = string(journal.WorkerConfigDivergenceDiverged)
	workerDivergenceNotChecked = string(journal.WorkerConfigDivergenceNotChecked)
	workerDivergenceNotActive  = string(journal.WorkerConfigDivergenceNotActive)
)

type workerDivergenceAppender = runtimewiring.WorkerDivergenceAppender

// workerDivergenceJournalRecorder makes retries idempotent against the
// daemon's durable journal, including after daemon restart. Only an exact
// repeat of the latest report for one worker is suppressed; A -> B -> A is
// three real transitions and remains three events.
type workerDivergenceJournalRecorder struct {
	mu                    sync.Mutex
	log                   *journal.InstanceLog
	latest                map[string]string
	reportingSentinelOpen bool
}

func newWorkerDivergenceJournalRecorder(log *journal.InstanceLog) (*workerDivergenceJournalRecorder, error) {
	if log == nil {
		return nil, errors.New("worker config divergence: instance journal is required")
	}
	recorder := &workerDivergenceJournalRecorder{log: log, latest: make(map[string]string)}
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		return nil, fmt.Errorf("worker config divergence: replay instance journal: %w", err)
	}
	for _, event := range events {
		if event.Type == journal.EventWorkerConfigDivergence {
			recorder.remember(event)
		}
	}
	return recorder, nil
}

func (r *workerDivergenceJournalRecorder) Append(event journal.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	worker := runtimewiring.DivergenceRunnerString(event, "worker")
	fingerprint := workerDivergenceFingerprint(event)
	resolvesSentinel := r.reportingSentinelOpen && worker != "" && worker != journal.WorkerConfigDivergenceReportingCapability
	if worker != "" && r.latest[worker] == fingerprint && !resolvesSentinel {
		return nil
	}
	if err := r.log.Append(event); err != nil {
		return err
	}
	r.remember(event)
	return nil
}

func (r *workerDivergenceJournalRecorder) remember(event journal.Event) {
	worker := runtimewiring.DivergenceRunnerString(event, "worker")
	// A real worker report resolves the daemon-lifetime capability sentinel.
	// Forget its fingerprint as well as letting the read-model fold hide it,
	// so the next daemon startup can author a genuinely new "awaiting" period.
	if worker == journal.WorkerConfigDivergenceReportingCapability {
		r.reportingSentinelOpen = true
	} else if worker != "" {
		r.reportingSentinelOpen = false
		delete(r.latest, journal.WorkerConfigDivergenceReportingCapability)
	}
	if worker != "" {
		r.latest[worker] = workerDivergenceFingerprint(event)
	}
}

func workerDivergenceFingerprint(event journal.Event) string {
	parts := []string{
		runtimewiring.DivergenceRunnerString(event, "state"),
		runtimewiring.DivergenceRunnerString(event, "workerDigest"),
		runtimewiring.DivergenceRunnerString(event, "daemonDigest"),
		runtimewiring.DivergenceRunnerString(event, "reason"),
		runtimewiring.DivergenceRunnerString(event, "message"),
	}
	var out strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&out, "%d:%s", len(part), part)
	}
	return out.String()
}

// recordDaemonWorkerDivergenceAvailability is the authoritative split-
// topology NOT ACTIVE path. A worker without a signing key cannot attest its
// hostname to the daemon, so the daemon records the unavailable capability
// itself instead of accepting an unauthenticated or stage-token write.
func recordDaemonWorkerDivergenceAvailability(recorder workerDivergenceAppender, cfg *instance.Config) error {
	if cfg == nil || !cfg.EngineProjectionEnabled() {
		return nil
	}
	state := workerDivergenceNotActive
	reason := "api.podTokenKeyFile is not configured"
	message := "worker config divergence: NOT ACTIVE — daemon has no api.podTokenKeyFile, so remote workers cannot durably attest config-tree state"
	if cfg.API.PodTokenKeyFile != "" {
		state = workerDivergenceNotChecked
		reason = "awaiting authenticated per-worker report"
		message = "worker config divergence: NOT CHECKED — remote reporting is active and awaiting authenticated per-worker state"
	}
	return recorder.Append(journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
		"worker": journal.WorkerConfigDivergenceReportingCapability, "state": state,
		"reason": reason, "message": message,
	}})
}
