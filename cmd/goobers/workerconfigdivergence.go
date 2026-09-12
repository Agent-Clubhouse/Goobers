package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
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
const workerDivergenceCheckInterval = time.Minute

// workerDigestTokenSource keeps static-token deployments compatible while a
// split worker uses its existing shared key for a separate, short-lived worker
// identity. Tokens are minted per request, never retained beyond a poll.
func workerDigestTokenSource(instanceRoot, staticToken string) (func() (string, error), error) {
	if staticToken != "" {
		return func() (string, error) { return staticToken, nil }, nil
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(instanceRoot).ConfigFile())
	if err != nil {
		return nil, err
	}
	signer, err := podTokenMinter(cfg)
	if err != nil || signer == nil {
		return nil, err
	}
	owner := workerDivergenceWorkerID()
	return func() (string, error) { return signer.MintWorkerConfigDigest(owner, 2*time.Minute) }, nil
}

// workerDivergenceReportTokenSource deliberately ignores GOOBERS_POD_TOKEN:
// that bearer authenticates a run's stage pod, not a resident worker host.
// Only the shared-key worker credential binds the hostname the daemon stamps
// into durable worker-health state.
func workerDivergenceReportTokenSource(instanceRoot string) (func() (string, error), error) {
	cfg, err := instance.LoadConfig(instance.NewLayout(instanceRoot).ConfigFile())
	if err != nil {
		return nil, err
	}
	signer, err := podTokenMinter(cfg)
	if err != nil || signer == nil {
		return nil, err
	}
	owner := workerDivergenceWorkerID()
	return func() (string, error) { return signer.MintWorkerConfigDigest(owner, 2*time.Minute) }, nil
}

func workerDivergenceWorkerID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "unknown-host"
	}
	return host
}

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

const (
	workerDivergenceInSync     = string(journal.WorkerConfigDivergenceInSync)
	workerDivergenceDiverged   = string(journal.WorkerConfigDivergenceDiverged)
	workerDivergenceNotChecked = string(journal.WorkerConfigDivergenceNotChecked)
	workerDivergenceNotActive  = string(journal.WorkerConfigDivergenceNotActive)
)

// State is the stable, machine-readable classification recorded in the
// instance journal. Message remains the full operator-facing report.
func (r divergenceReport) State() string {
	switch {
	case r.Unavailable != "":
		return workerDivergenceNotChecked
	case r.Diverged:
		return workerDivergenceDiverged
	default:
		return workerDivergenceInSync
	}
}

type workerDivergenceAppender interface {
	Append(journal.Event) error
}

type remoteWorkerDivergenceAppender struct {
	client      *http.Client
	baseURL     string
	tokenSource func() (string, error)
}

type localWorkerDivergenceAppender struct{ layout instance.Layout }

func (a localWorkerDivergenceAppender) Append(event journal.Event) error {
	log, _, err := journal.OpenInstanceLog(a.layout.SchedulerDir())
	if err != nil {
		return err
	}
	if err := log.Append(event); err != nil {
		_ = log.Close()
		return err
	}
	return log.Close()
}

func (a *remoteWorkerDivergenceAppender) Append(event journal.Event) error {
	body, err := json.Marshal(map[string]string{
		"state":        divergenceRunnerString(event, "state"),
		"workerDigest": divergenceRunnerString(event, "workerDigest"),
		"daemonDigest": divergenceRunnerString(event, "daemonDigest"),
		"reason":       divergenceRunnerString(event, "reason"),
	})
	if err != nil {
		return fmt.Errorf("marshal worker config-divergence event: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(a.baseURL, "/")+apicontract.WorkerConfigDivergencePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if a.tokenSource != nil {
		token, err := a.tokenSource()
		if err != nil {
			return fmt.Errorf("mint worker config-divergence credential: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := a.client
	if client == nil {
		client = http.DefaultClient
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := boundedClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("daemon worker config-divergence plane returned %s", response.Status)
	}
	return nil
}

func divergenceRunnerString(event journal.Event, key string) string {
	value, _ := event.Runner[key].(string)
	return value
}

func recordWorkerDivergence(appender workerDivergenceAppender, worker, state string, report divergenceReport, message string) error {
	return appender.Append(journal.Event{
		Type: journal.EventWorkerConfigDivergence,
		Runner: map[string]any{
			"worker":       worker,
			"state":        state,
			"workerDigest": report.WorkerDigest,
			"daemonDigest": report.DaemonDigest,
			"reason":       report.Unavailable,
			"message":      message,
		},
	})
}

func recordInactiveWorkerDivergence(appender workerDivergenceAppender, worker, workerDigest string) (string, error) {
	message := "worker config divergence: NOT ACTIVE — configure api.podTokenKeyFile; " +
		"this worker cannot durably report its config-tree state to the daemon (#4153)"
	return message, recordWorkerDivergence(appender, worker, workerDivergenceNotActive, divergenceReport{WorkerDigest: workerDigest}, message)
}

// workerDivergenceJournalRecorder makes retries idempotent against the
// daemon's durable journal, including after daemon restart. Only an exact
// repeat of the latest report for one worker is suppressed; A -> B -> A is
// three real transitions and remains three events.
type workerDivergenceJournalRecorder struct {
	mu     sync.Mutex
	log    *journal.InstanceLog
	latest map[string]string
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
	worker := divergenceRunnerString(event, "worker")
	fingerprint := workerDivergenceFingerprint(event)
	if worker != "" && r.latest[worker] == fingerprint {
		return nil
	}
	if err := r.log.Append(event); err != nil {
		return err
	}
	r.remember(event)
	return nil
}

func (r *workerDivergenceJournalRecorder) remember(event journal.Event) {
	worker := divergenceRunnerString(event, "worker")
	if worker != "" {
		r.latest[worker] = workerDivergenceFingerprint(event)
	}
}

func workerDivergenceFingerprint(event journal.Event) string {
	parts := []string{
		divergenceRunnerString(event, "state"),
		divergenceRunnerString(event, "workerDigest"),
		divergenceRunnerString(event, "daemonDigest"),
		divergenceRunnerString(event, "reason"),
		divergenceRunnerString(event, "message"),
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

func configureWorkerDivergence(ctx context.Context, seams *workerSeams, instanceRoot, daemonAPI string, stdout, stderr io.Writer) (*workerConfigWatcher, error) {
	workerIdentity := "worker:" + workerDivergenceWorkerID()
	digestTokenSource, err := workerDigestTokenSource(instanceRoot, workerEnvOr("GOOBERS_POD_TOKEN", ""))
	if err != nil {
		return nil, err
	}
	reportTokenSource, err := workerDivergenceReportTokenSource(instanceRoot)
	if err != nil {
		return nil, err
	}
	if reportTokenSource == nil {
		// The local instance journal is the only trustworthy no-credential
		// write path. In a split topology this copy may not be daemon-owned;
		// the daemon independently records that remote reporting is unavailable
		// when it has no worker signing key.
		localAppender := localWorkerDivergenceAppender{layout: instance.NewLayout(instanceRoot)}
		_, openErr := recordInactiveWorkerDivergence(localAppender, workerIdentity, seams.currentDigest())
		if openErr != nil {
			pf(stderr, "warning: goobers worker: record config-divergence state: %v\n", openErr)
		}
		pf(stderr, "warning: goobers worker: config-divergence reporting is NOT ACTIVE — configure api.podTokenKeyFile; "+
			"GOOBERS_POD_TOKEN authenticates a stage run and cannot attest worker health (#4153)\n")
		if digestTokenSource == nil {
			return nil, nil
		}
		watcher := startWorkerDivergenceWatcher(ctx, seams, http.DefaultClient, daemonAPI, digestTokenSource,
			workerDivergenceCheckInterval, workerIdentity, localAppender)
		pf(stdout, "goobers worker: checking config-tree divergence against %s every %s; durable remote reporting is not active\n",
			daemonAPI, workerDivergenceCheckInterval)
		return watcher, nil
	}
	appender := &remoteWorkerDivergenceAppender{client: http.DefaultClient, baseURL: daemonAPI, tokenSource: reportTokenSource}
	watcher := startWorkerDivergenceWatcher(ctx, seams, http.DefaultClient, daemonAPI, digestTokenSource,
		workerDivergenceCheckInterval, workerIdentity, appender)
	pf(stdout, "goobers worker: checking config-tree divergence against %s every %s\n", daemonAPI, workerDivergenceCheckInterval)
	return watcher, nil
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
	// A config endpoint relocation must not forward a worker credential.
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := boundedClient.Do(request)
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
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return "", fmt.Errorf("read config-digest response: %w", err)
	}
	if len(body) > 4096 {
		return "", fmt.Errorf("config-digest response exceeds 4096 bytes")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
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
	baseURL string,
	tokenSource func() (string, error),
	interval time.Duration,
	worker string,
	appender workerDivergenceAppender,
) *workerConfigWatcher {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	watcher := &workerConfigWatcher{cancel: cancel, done: done}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastLogged, lastRecorded string
		var pending *journal.Event
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Preserve transition ordering across a failed delivery: retry the
				// old state before observing and publishing a newer one. The daemon
				// suppresses an exact retry durably, covering a lost response.
				if pending != nil {
					if err := appender.Append(*pending); err != nil {
						seams.log("worker config divergence: journal transition: %v", err)
						continue
					}
					lastRecorded = divergenceRunnerString(*pending, "message")
					pending = nil
				}
				requestCtx, cancelRequest := context.WithTimeout(ctx, 15*time.Second)
				token, err := tokenSource()
				var daemonDigest string
				if err == nil {
					daemonDigest, err = fetchDaemonConfigDigest(requestCtx, client, baseURL, token)
				}
				cancelRequest()
				report := compareConfigDigests(seams.currentDigest(), daemonDigest, err)
				message := report.Message()
				if message != lastLogged {
					lastLogged = message
					seams.log("%s", message)
				}
				if message != lastRecorded {
					event := journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
						"worker": worker, "state": report.State(),
						"workerDigest": report.WorkerDigest, "daemonDigest": report.DaemonDigest,
						"reason": report.Unavailable, "message": message,
					}}
					if err := appender.Append(event); err != nil {
						seams.log("worker config divergence: journal transition: %v", err)
						pending = &event
						continue
					}
					lastRecorded = message
				}
			}
		}
	}()
	return watcher
}
