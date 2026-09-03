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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
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
	Schema          string              `json:"schema"`
	Revision        uint64              `json:"revision"`
	ActionsRunID    string              `json:"actionsRunId"`
	ActionsRunURL   string              `json:"actionsRunUrl"`
	Identity        journal.RunIdentity `json:"identity"`
	Phase           journal.RunPhase    `json:"phase"`
	Graph           json.RawMessage     `json:"graph,omitempty"`
	Events          []journal.Event     `json:"events"`
	UpdatedAt       time.Time           `json:"updatedAt"`
	TruncatedBefore uint64              `json:"truncatedBefore,omitempty"`
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

// Publisher owns one Check Run and updates it only when the journal advances.
type Publisher struct {
	env       GitHubEnvironment
	client    *http.Client
	runDir    string
	checkID   int64
	lastSeq   uint64
	disabled  error
	finalized bool
	mu        sync.Mutex
}

// New creates a publisher. It performs no network operation until Publish.
func New(env GitHubEnvironment, runDir string) *Publisher {
	return &Publisher{
		env:    env,
		client: &http.Client{Timeout: 15 * time.Second},
		runDir: runDir,
	}
}

// Publish projects the complete committed journal prefix. Duplicate calls for
// the same sequence are free and make the 200ms run watcher safe.
func (p *Publisher) Publish(ctx context.Context, events []journal.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled != nil {
		return p.disabled
	}
	revision := latestProjectedSequence(events)
	if revision == 0 || revision <= p.lastSeq {
		return nil
	}
	contract, err := p.contract(events)
	if err != nil {
		return err
	}
	if p.checkID == 0 {
		p.checkID, err = p.create(ctx, contract)
	} else {
		err = p.update(ctx, contract)
	}
	if err != nil {
		p.disabled = err
		return err
	}
	p.lastSeq = contract.Revision
	if terminal(contract.Phase) {
		p.finalized = true
	}
	return nil
}

// Finalize closes the Check Run for the current run when the caller exits
// without observing a terminal journal phase (context cancellation, timeout,
// or wait error). It is a no-op when Publish was never able to create a
// Check Run and when Publish has already published a terminal phase. On
// success the Check Run is marked completed so it does not linger as
// "in progress" after the workflow job ends. Failures are returned but
// callers should treat them as best-effort — the run is already over.
func (p *Publisher) Finalize(ctx context.Context, waitErr error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checkID == 0 || p.finalized {
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

// finalizeConclusion maps a wait-error into a GitHub Check Run conclusion.
// A cancelled context (Ctrl-C, cancelled Actions job, deadline) becomes
// "cancelled"; any other failure becomes "failure". Callers passing a nil
// error (i.e. the wait exited abnormally without an explicit error) get
// "cancelled" as the least-alarming terminal marker.
func finalizeConclusion(waitErr error) string {
	if waitErr == nil {
		return "cancelled"
	}
	if errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "failure"
}

func (p *Publisher) contract(events []journal.Event) (Contract, error) {
	reader, err := journal.OpenRead(p.runDir)
	if err != nil {
		return Contract{}, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return Contract{}, err
	}
	var graph json.RawMessage
	for _, input := range identity.Inputs {
		if input.Name != journal.PinnedWorkflowGraphInputName {
			continue
		}
		raw, readErr := reader.ArtifactBytes(input.Ref)
		if readErr != nil {
			return Contract{}, readErr
		}
		if !json.Valid(raw) {
			return Contract{}, errors.New("hosted progress: pinned workflow graph is not valid JSON")
		}
		graph = raw
		break
	}
	contract := Contract{
		Schema:        Schema,
		Revision:      latestProjectedSequence(events),
		ActionsRunID:  p.env.ActionsRunID,
		ActionsRunURL: strings.TrimRight(p.env.ServerURL, "/") + "/" + p.env.Repository + "/actions/runs/" + p.env.ActionsRunID,
		Identity:      identity,
		Phase:         journal.PhaseFromEvents(events),
		Graph:         graph,
		Events:        projectEvents(events),
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
			contract.Events = nil
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
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
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

func latestProjectedSequence(events []journal.Event) uint64 {
	projected := projectEvents(events)
	if len(projected) == 0 {
		return 0
	}
	return projected[len(projected)-1].Seq
}

type checkOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text"`
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
		ExternalID: Schema + ":" + contract.Identity.RunID + ":" + p.env.ActionsRunID,
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
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(p.env.APIURL, "/")+endpoint,
		bytes.NewReader(raw),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+p.env.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("publish GitHub progress: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if response != nil {
		return json.NewDecoder(resp.Body).Decode(response)
	}
	return nil
}
