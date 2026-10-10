// Package hostedprogress publishes a bounded, versioned projection of a live
// Goobers journal to a GitHub Check Run for remote portal clients.
package hostedprogress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/truncate"
)

const (
	// Schema is the versioned identifier for the hosted-progress payload
	// embedded in a GitHub Check Run output.
	Schema = "goobers.dev/hosted-progress/v1"
	// CheckPrefix is prepended to the GitHub Check Run name so the hosted
	// progress publisher can locate its own runs and callers can filter them.
	CheckPrefix     = "Goobers / "
	startMarker     = "<!-- goobers-progress:v1 -->"
	endMarker       = "<!-- /goobers-progress:v1 -->"
	maxPayloadBytes = 56000
)

// Contract is the portable live-run projection embedded in a GitHub Check Run.
// It intentionally reuses the canonical journal identity, graph, and events.
type Contract struct {
	Schema        string              `json:"schema"`
	Revision      uint64              `json:"revision"`
	ActionsRunID  string              `json:"actionsRunId"`
	ActionsRunURL string              `json:"actionsRunUrl"`
	Identity      journal.RunIdentity `json:"identity"`
	Phase         journal.RunPhase    `json:"phase"`
	Graph         json.RawMessage     `json:"graph,omitempty"`
	Events        []journal.Event     `json:"events"`
	UpdatedAt     time.Time           `json:"updatedAt"`
	// TruncatedBefore is the highest journal sequence dropped by payload
	// bounding (see boundContract). When non-zero, the retained anchor
	// Events[0] is preserved regardless of its sequence, every later
	// projected event with Seq > TruncatedBefore is present in Events, and
	// events with a sequence at or below TruncatedBefore (other than the
	// anchor) may have been dropped. When Events is empty this equals
	// Revision, marking that every projected event was dropped.
	TruncatedBefore uint64 `json:"truncatedBefore,omitempty"`
}

// GitHubEnvironment is the workflow metadata required to publish progress.
type GitHubEnvironment struct {
	Repository   string
	SHA          string
	ActionsRunID string
	Token        string
	APIURL       string
	ServerURL    string
}

// Environment reads the stable GitHub Actions contract. Workflows opt in by
// passing --github-progress and exposing github.token as GITHUB_TOKEN.
func Environment() (GitHubEnvironment, error) {
	env := GitHubEnvironment{
		Repository:   os.Getenv("GITHUB_REPOSITORY"),
		SHA:          os.Getenv("GITHUB_SHA"),
		ActionsRunID: os.Getenv("GITHUB_RUN_ID"),
		Token:        os.Getenv("GITHUB_TOKEN"),
		APIURL:       envOr("GITHUB_API_URL", "https://api.github.com"),
		ServerURL:    envOr("GITHUB_SERVER_URL", "https://github.com"),
	}
	var missing []string
	if env.Repository == "" {
		missing = append(missing, "GITHUB_REPOSITORY")
	}
	if env.SHA == "" {
		missing = append(missing, "GITHUB_SHA")
	}
	if env.ActionsRunID == "" {
		missing = append(missing, "GITHUB_RUN_ID")
	}
	if env.Token == "" {
		missing = append(missing, "GITHUB_TOKEN")
	}
	if len(missing) > 0 {
		return GitHubEnvironment{}, fmt.Errorf(
			"github progress requires %s (set permissions: checks: write and expose github.token as GITHUB_TOKEN)",
			strings.Join(missing, ", "),
		)
	}
	return env, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

const (
	retryBaseDelay = time.Second
	retryMaxDelay  = time.Minute
)

// Publisher owns one Check Run and updates it only when the journal advances.
type Publisher struct {
	env    GitHubEnvironment
	client *http.Client
	runDir string
	now    func() time.Time
	// checkID, lastSeq, disabled, finalized, failures, retryAt and lastErr
	// are guarded by mu.
	checkID   int64
	lastSeq   uint64
	disabled  error
	finalized bool
	failures  int
	retryAt   time.Time
	lastErr   error
	// identity and graph are immutable for a run and loaded once.
	identity journal.RunIdentity
	graph    json.RawMessage
	loaded   bool
	mu       sync.Mutex
}

// New creates a publisher. It performs no network operation until Publish.
func New(env GitHubEnvironment, runDir string) *Publisher {
	return &Publisher{
		env:    env,
		client: &http.Client{Timeout: 15 * time.Second},
		runDir: runDir,
		now:    time.Now,
	}
}

// PermanentError marks a publish failure that retrying cannot fix (bad
// credentials, missing checks: write permission, unknown repository or a
// rejected payload). The Publisher stops publishing after one.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is a publish failure that disabled the
// Publisher; any other publish error is transient and retried with backoff.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

// statusError is a non-2xx GitHub API response.
type statusError struct {
	code        int
	rateLimited bool
	detail      string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("publish GitHub progress: HTTP %d: %s", e.code, e.detail)
}

func (e *statusError) permanent() bool {
	switch e.code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusGone, http.StatusUnprocessableEntity:
		return true
	case http.StatusForbidden:
		return !e.rateLimited
	}
	return false
}

// Publish projects the complete committed journal prefix. Duplicate calls for
// the same sequence are free and make the 200ms run watcher safe. A
// transient GitHub failure (network error, timeout, 5xx, rate limit) is
// returned and retried on a later call after an exponential backoff, so one
// blip does not stop publishing for the rest of the run; only a
// PermanentError disables the Publisher.
func (p *Publisher) Publish(ctx context.Context, events []journal.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled != nil {
		return p.disabled
	}
	if p.now().Before(p.retryAt) {
		return p.lastErr
	}
	return p.publishLocked(ctx, events)
}

func (p *Publisher) publishLocked(ctx context.Context, events []journal.Event) error {
	projected := projectEvents(events)
	if len(projected) == 0 {
		return nil
	}
	revision := projected[len(projected)-1].Seq
	if revision <= p.lastSeq {
		return nil
	}
	contract, err := p.contract(events, projected, revision)
	if err != nil {
		return err
	}
	if err := p.write(ctx, contract); err != nil {
		return p.recordFailure(err)
	}
	p.failures, p.retryAt, p.lastErr = 0, time.Time{}, nil
	p.lastSeq = contract.Revision
	if terminal(contract.Phase) {
		p.finalized = true
	}
	return nil
}

func (p *Publisher) write(ctx context.Context, contract Contract) error {
	if p.checkID != 0 {
		return p.update(ctx, contract)
	}
	// A fresh Publisher (process restart, retry, or the very first Publish
	// for this run) has no in-memory checkID. Look up an existing Check Run
	// for our stable external_id before creating, so a repeat run against
	// the same (SHA, run, actions run) does not fan out to duplicate GitHub
	// Check Runs. A failed lookup never falls through to create. See
	// TestPublisherReusesExistingCheckRunAcrossPublishers.
	existing, err := p.findExisting(ctx, contract)
	if err != nil {
		return err
	}
	if existing != 0 {
		p.checkID = existing
		return p.update(ctx, contract)
	}
	p.checkID, err = p.create(ctx, contract)
	return err
}

// recordFailure disables the Publisher on a permanent error and otherwise
// schedules the next attempt with capped exponential backoff.
func (p *Publisher) recordFailure(err error) error {
	var status *statusError
	if errors.As(err, &status) && status.permanent() {
		p.disabled = &PermanentError{Err: err}
		return p.disabled
	}
	delay := retryMaxDelay
	if p.failures < 6 {
		delay = min(retryBaseDelay<<p.failures, retryMaxDelay)
	}
	p.failures++
	p.retryAt = p.now().Add(delay)
	p.lastErr = err
	return err
}

// Finalize closes the Check Run for the current run when the caller exits
// without a terminal phase having been published (context cancellation,
// timeout, wait error, or a terminal publish lost to a transient failure).
// It is a no-op when Publish has already published a terminal phase, or when
// a permanent failure disabled the Publisher before any Check Run existed. When
// the journal on disk is terminal, Finalize publishes it so the Check Run
// carries the run's real conclusion (a completed run is never mislabelled
// "cancelled"); otherwise it marks an existing Check Run completed with
// finalizeConclusion(waitErr), and is a no-op when no Check Run was ever
// created. Failures are returned but callers should treat them as
// best-effort — the run is already over.
func (p *Publisher) Finalize(ctx context.Context, waitErr error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A permanent publish failure still leaves an existing Check Run to close.
	if p.finalized || (p.disabled != nil && p.checkID == 0) {
		return nil
	}
	if events, ok := p.terminalEvents(); ok {
		return p.publishLocked(ctx, events)
	}
	if p.checkID == 0 {
		return nil
	}
	p.finalized = true
	body := struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Completed  string `json:"completed_at"`
	}{
		Status:     "completed",
		Conclusion: finalizeConclusion(waitErr),
		Completed:  time.Now().UTC().Format(time.RFC3339),
	}
	return p.request(
		ctx,
		http.MethodPatch,
		fmt.Sprintf("/repos/%s/check-runs/%d", p.env.Repository, p.checkID),
		body,
		nil,
	)
}

// terminalEvents returns the run's journal when it has reached a terminal
// phase.
func (p *Publisher) terminalEvents() ([]journal.Event, bool) {
	reader, err := journal.OpenRead(p.runDir)
	if err != nil {
		return nil, false
	}
	events, err := reader.Events()
	if err != nil || !terminal(journal.PhaseFromEvents(events)) {
		return nil, false
	}
	return events, true
}

// finalizeConclusion maps a wait-error for a run whose journal is not
// terminal into a GitHub Check Run conclusion. A cancelled context (Ctrl-C,
// cancelled Actions job, deadline) becomes "cancelled"; any other failure
// becomes "failure". A nil error means the wait stopped without an explicit
// error before the run finished, so "cancelled" is the least-alarming
// terminal marker. Terminal journals never reach this mapping; Finalize
// publishes their real conclusion instead.
func finalizeConclusion(waitErr error) string {
	if waitErr == nil {
		return "cancelled"
	}
	if errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "failure"
}

func (p *Publisher) loadStatic() error {
	if p.loaded {
		return nil
	}
	reader, err := journal.OpenRead(p.runDir)
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	var graph json.RawMessage
	for _, input := range identity.Inputs {
		if input.Name != journal.PinnedWorkflowGraphInputName {
			continue
		}
		raw, readErr := reader.ArtifactBytes(input.Ref)
		if readErr != nil {
			return readErr
		}
		if !json.Valid(raw) {
			return errors.New("hosted progress: pinned workflow graph is not valid JSON")
		}
		graph = raw
		break
	}
	p.identity, p.graph, p.loaded = identity, graph, true
	return nil
}

func (p *Publisher) contract(events, projected []journal.Event, revision uint64) (Contract, error) {
	if err := p.loadStatic(); err != nil {
		return Contract{}, err
	}
	contract := Contract{
		Schema:        Schema,
		Revision:      revision,
		ActionsRunID:  p.env.ActionsRunID,
		ActionsRunURL: strings.TrimRight(p.env.ServerURL, "/") + "/" + p.env.Repository + "/actions/runs/" + p.env.ActionsRunID,
		Identity:      p.identity,
		Phase:         journal.PhaseFromEvents(events),
		Graph:         p.graph,
		Events:        projected,
		UpdatedAt:     time.Now().UTC(),
	}
	boundContract(&contract)
	return contract, nil
}

func boundContract(contract *Contract) {
	for {
		raw, err := json.Marshal(contract)
		if err != nil || len(raw) <= maxPayloadBytes {
			return
		}
		switch {
		case len(contract.Events) > 1:
			contract.TruncatedBefore = contract.Events[1].Seq
			contract.Events = append(contract.Events[:1], contract.Events[2:]...)
		case contract.Graph != nil:
			contract.Graph = nil
		case len(contract.Events) == 1:
			contract.Events = []journal.Event{compactEvent(contract.Events[0])}
			raw, err := json.Marshal(contract)
			if err == nil && len(raw) <= maxPayloadBytes {
				return
			}
			// Empty non-nil slice: nil would marshal to `"events": null`,
			// which violates the schema's required "type": "array" for
			// events. See TestBoundContractMarksAllEventsDropped and
			// TestHostedProgressAllEventsDroppedContractValidates.
			contract.Events = []journal.Event{}
			contract.TruncatedBefore = contract.Revision
		default:
			return
		}
	}
}

func compactEvent(event journal.Event) journal.Event {
	compact := journal.Event{
		Schema:       event.Schema,
		Seq:          event.Seq,
		Type:         event.Type,
		Branch:       event.Branch,
		Time:         event.Time,
		Stage:        boundedString(event.Stage),
		Attempt:      event.Attempt,
		Gate:         boundedString(event.Gate),
		Verdict:      boundedString(event.Verdict),
		Target:       boundedString(event.Target),
		Status:       boundedString(event.Status),
		Parallel:     boundedString(event.Parallel),
		BranchName:   boundedString(event.BranchName),
		BranchStatus: event.BranchStatus,
	}
	if event.ExternalRef != nil {
		compact.ExternalRef = &journal.ExternalRef{
			Provider: boundedString(event.ExternalRef.Provider),
			Kind:     boundedString(event.ExternalRef.Kind),
			ID:       boundedString(event.ExternalRef.ID),
			URL:      boundedString(event.ExternalRef.URL),
		}
	}
	if event.Error != nil {
		compact.Error = &journal.ErrorDetail{
			Code:    boundedString(event.Error.Code),
			Message: boundedString(event.Error.Message),
		}
	}
	return compact
}

func boundedString(value string) string {
	const limit = 1024
	return truncate.Bytes(value, limit, "...")
}

func projectEvents(events []journal.Event) []journal.Event {
	projected := make([]journal.Event, 0, len(events))
	for _, event := range events {
		switch event.Type {
		case journal.EventRunStarted,
			journal.EventRunResumed,
			journal.EventRunFinished,
			journal.EventStageStarted,
			journal.EventStageFinished,
			journal.EventGateStarted,
			journal.EventGatePaused,
			journal.EventGateEvaluated,
			journal.EventGateOverridden,
			journal.EventRefTouched,
			journal.EventError,
			journal.EventParallelStarted,
			journal.EventBranchStarted,
			journal.EventBranchFinished,
			journal.EventParallelFinished:
			projected = append(projected, event)
		}
	}
	return projected
}

type checkOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text"`
}

func (p *Publisher) externalID(contract Contract) string {
	return Schema + ":" + contract.Identity.RunID + ":" + p.env.ActionsRunID
}

// findExisting looks up a previously-created Check Run for this run whose
// external_id matches the stable identifier this Publisher would use, so a
// fresh Publisher (process restart, retry, or a resumed run) reuses that
// Check Run instead of creating a duplicate. Returns 0 with a nil error when
// no matching Check Run exists on this SHA.
func (p *Publisher) findExisting(ctx context.Context, contract Contract) (int64, error) {
	checkName := CheckPrefix + contract.Identity.Workflow
	// The Check Runs "list for a Git ref" endpoint is the narrowest server-
	// side filter available: it scopes to a single commit SHA and a single
	// check name. GitHub does not expose a filter on external_id itself, so
	// we scan the returned list client-side. per_page=100 (the API cap) is
	// enough headroom for a hosted-progress run: this Publisher only ever
	// contributes one Check Run per (SHA, run, actions run).
	endpoint := fmt.Sprintf(
		"/repos/%s/commits/%s/check-runs?check_name=%s&per_page=100",
		p.env.Repository,
		url.PathEscape(p.env.SHA),
		url.QueryEscape(checkName),
	)
	var response struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			ExternalID string `json:"external_id"`
		} `json:"check_runs"`
	}
	if err := p.request(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return 0, err
	}
	want := p.externalID(contract)
	for _, run := range response.CheckRuns {
		if run.ExternalID == want {
			return run.ID, nil
		}
	}
	return 0, nil
}

func (p *Publisher) create(ctx context.Context, contract Contract) (int64, error) {
	body := struct {
		Name       string      `json:"name"`
		HeadSHA    string      `json:"head_sha"`
		Status     string      `json:"status"`
		Conclusion string      `json:"conclusion,omitempty"`
		Completed  string      `json:"completed_at,omitempty"`
		DetailsURL string      `json:"details_url"`
		ExternalID string      `json:"external_id"`
		Output     checkOutput `json:"output"`
	}{
		Name:       CheckPrefix + contract.Identity.Workflow,
		HeadSHA:    p.env.SHA,
		Status:     "in_progress",
		DetailsURL: contract.ActionsRunURL,
		ExternalID: p.externalID(contract),
		Output:     outputFor(contract),
	}
	if terminal(contract.Phase) {
		body.Status = "completed"
		body.Conclusion = conclusion(contract.Phase)
		body.Completed = contract.UpdatedAt.Format(time.RFC3339)
	}
	var response struct {
		ID int64 `json:"id"`
	}
	if err := p.request(ctx, http.MethodPost, "/repos/"+p.env.Repository+"/check-runs", body, &response); err != nil {
		return 0, err
	}
	return response.ID, nil
}

func (p *Publisher) update(ctx context.Context, contract Contract) error {
	body := struct {
		Status     string      `json:"status"`
		Conclusion string      `json:"conclusion,omitempty"`
		Completed  string      `json:"completed_at,omitempty"`
		Output     checkOutput `json:"output"`
	}{
		Status: "in_progress",
		Output: outputFor(contract),
	}
	if terminal(contract.Phase) {
		body.Status = "completed"
		body.Conclusion = conclusion(contract.Phase)
		body.Completed = contract.UpdatedAt.Format(time.RFC3339)
	}
	return p.request(
		ctx,
		http.MethodPatch,
		fmt.Sprintf("/repos/%s/check-runs/%d", p.env.Repository, p.checkID),
		body,
		nil,
	)
}

func outputFor(contract Contract) checkOutput {
	raw, _ := json.Marshal(contract)
	return checkOutput{
		Title:   fmt.Sprintf("%s · %s", contract.Identity.Workflow, contract.Phase),
		Summary: fmt.Sprintf("Goobers run `%s` is **%s**. Live progress contract revision %d.", contract.Identity.RunID, contract.Phase, contract.Revision),
		Text:    startMarker + "\n```json\n" + string(raw) + "\n```\n" + endMarker,
	}
}

func terminal(phase journal.RunPhase) bool {
	return phase == journal.PhaseCompleted ||
		phase == journal.PhaseFailed ||
		phase == journal.PhaseAborted ||
		phase == journal.PhaseEscalated
}

func conclusion(phase journal.RunPhase) string {
	if phase == journal.PhaseCompleted {
		return "success"
	}
	if phase == journal.PhaseAborted {
		return "cancelled"
	}
	if phase == journal.PhaseEscalated {
		return "action_required"
	}
	return "failure"
}

func (p *Publisher) request(ctx context.Context, method, endpoint string, body, response any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(p.env.APIURL, "/")+endpoint,
		reader,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+p.env.Token)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return &statusError{
			code: resp.StatusCode,
			rateLimited: resp.Header.Get("Retry-After") != "" ||
				resp.Header.Get("X-RateLimit-Remaining") == "0" ||
				strings.Contains(strings.ToLower(string(detail)), "rate limit"),
			detail: strings.TrimSpace(string(detail)),
		}
	}
	if response != nil {
		return json.NewDecoder(resp.Body).Decode(response)
	}
	return nil
}
