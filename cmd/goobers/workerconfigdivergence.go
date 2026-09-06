package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
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
const workerDivergenceCheckInterval = time.Minute

// divergenceReport is one comparison's outcome.
type divergenceReport struct {
	// DaemonDigest is the tree the daemon reports in force, empty when it
	// could not be read.
	DaemonDigest string
	// WorkerDigest is the tree this worker currently serves.
	WorkerDigest string
	// Diverged is true only when BOTH digests are known and differ. An
	// unreadable daemon digest is never divergence: reporting one on a failed
	// request would cry wolf on every restart and network blip, and an alarm
	// that fires for the wrong reason stops being read.
	Diverged bool
	// Unavailable explains why no comparison could be made.
	Unavailable string
}

// Message renders the report for an operator.
func (r divergenceReport) Message() string {
	switch {
	case r.Unavailable != "":
		return fmt.Sprintf("worker config divergence: NOT CHECKED (%s); this worker cannot tell whether its config tree matches the daemon's", r.Unavailable)
	case r.Diverged:
		return fmt.Sprintf("worker config divergence: this worker serves config tree %s but the daemon has %s in force. "+
			"Every agentic gate is pinned to the daemon's tree and served from this one, so gates will be REFUSED "+
			"(gate_pin_missing) until they agree. The worker's tree is seeded at deploy time and has no live writer: "+
			"a goober-content change merged to the config repo requires a DEPLOY, not just a merge (#4153)",
			r.WorkerDigest, r.DaemonDigest)
	default:
		return fmt.Sprintf("worker config divergence: none; worker and daemon both have config tree %s in force", r.WorkerDigest)
	}
}

// fetchDaemonConfigDigest reads the daemon's current config-tree digest.
func fetchDaemonConfigDigest(ctx context.Context, client *http.Client, baseURL, token string) (string, error) {
	endpoint := strings.TrimSuffix(baseURL, "/") + apicontract.ConfigDigestPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon config-digest plane returned %s", response.Status)
	}
	var payload struct {
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode config-digest response: %w", err)
	}
	if payload.Digest == "" {
		return "", fmt.Errorf("daemon reported an empty config-tree digest")
	}
	return payload.Digest, nil
}

// compareConfigDigests builds the report for one comparison.
func compareConfigDigests(workerDigest, daemonDigest string, fetchErr error) divergenceReport {
	report := divergenceReport{WorkerDigest: workerDigest, DaemonDigest: daemonDigest}
	switch {
	case fetchErr != nil:
		report.Unavailable = fetchErr.Error()
	case workerDigest == "":
		// The worker has not published a snapshot yet. Not divergence: it has
		// no position to diverge from.
		report.Unavailable = "this worker has not resolved its own config tree yet"
	case daemonDigest != workerDigest:
		report.Diverged = true
	}
	return report
}

// startWorkerDivergenceWatcher polls the daemon and reports divergence until
// Stop, following startWorkerConfigWatcher's lifecycle shape (own context, own
// done channel, Stop waits) so shutdown is an observable fact.
//
// Reporting is DEDUPED by message, exactly like the reload watcher's
// lastFailure: a divergence persists until an operator deploys, and repeating
// the same line every minute for hours would bury it in its own noise. It says
// so once, again if it changes, and once more when it clears — the last being
// the line that tells an operator their deploy worked.
func startWorkerDivergenceWatcher(
	ctx context.Context,
	seams *workerSeams,
	client *http.Client,
	baseURL, token string,
	interval time.Duration,
) *workerConfigWatcher {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	watcher := &workerConfigWatcher{cancel: cancel, done: done}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var last string
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				requestCtx, cancelRequest := context.WithTimeout(ctx, 15*time.Second)
				daemonDigest, err := fetchDaemonConfigDigest(requestCtx, client, baseURL, token)
				cancelRequest()
				report := compareConfigDigests(seams.currentDigest(), daemonDigest, err)
				message := report.Message()
				if message == last {
					continue
				}
				last = message
				seams.log("%s", message)
			}
		}
	}()
	return watcher
}
