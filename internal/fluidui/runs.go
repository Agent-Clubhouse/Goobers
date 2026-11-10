package fluidui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
)

// Workflow stage names the daemon reads results from.
const (
	LoadStage     = "load-request"
	ValidateStage = "validate-candidate"
	// InputSchema versions the load-request stage's result file.
	InputSchema = "fluid-ui-request/v1"
)

const maxResultBytes = 64 << 10

// RunState is what the daemon can observe of one run. An empty Phase means no
// run journal exists yet.
type RunState struct {
	Phase  journal.RunPhase
	Result *CandidateResult
}

// RunReader observes a run by ID.
type RunReader func(runID string) (RunState, error)

// JournalRuns observes runs through their journals under layout. The result
// is the newest successful validate-candidate result file, so a run that
// retried reports its latest attempt.
func JournalRuns(layout instance.Layout) RunReader {
	return func(runID string) (RunState, error) {
		file, err := journalclient.OpenFile(layout, runID)
		if errors.Is(err, journalclient.ErrRunNotFound) {
			return RunState{}, nil
		}
		if err != nil {
			return RunState{}, err
		}
		phase, err := file.Phase()
		if err != nil {
			return RunState{}, err
		}
		state := RunState{Phase: phase}
		content, err := journalclient.StageArtifactContent(file, ValidateStage, "/result", journalclient.ArtifactBounds{MaxBytes: maxResultBytes})
		if err == nil {
			var result CandidateResult
			if json.Unmarshal(content.Bytes, &result) == nil {
				state.Result = &result
			}
		}
		return state, nil
	}
}

func (r *Requests) refreshAll(ctx context.Context, user string, file *requestFile, trigger Trigger) bool {
	changed := false
	for i := range file.Requests {
		if r.refresh(ctx, user, &file.Requests[i], trigger) {
			changed = true
		}
	}
	return changed
}

// refresh derives an in-flight request's status from its run. Statuses past
// the run (ready_to_preview and later) are never touched here.
func (r *Requests) refresh(ctx context.Context, user string, rec *storedRequest, trigger Trigger) bool {
	if rec.Status != StatusQueued && rec.Status != StatusImplementing || r.runs == nil {
		return false
	}
	state, err := r.runs(rec.RunID)
	if err != nil {
		return false
	}
	switch state.Phase {
	case "":
		return r.refreshAcceptance(ctx, rec, trigger)
	case journal.PhaseRunning:
		if rec.Status == StatusImplementing {
			return false
		}
		rec.Status, rec.UpdatedAt = StatusImplementing, r.now()
		return true
	case journal.PhaseCompleted:
		r.conclude(user, rec, state.Result)
	default:
		r.fail(rec, fmt.Sprintf("the customization run ended %s", state.Phase))
		if state.Result != nil {
			rec.Errors = state.Result.Errors
		}
	}
	return true
}

// refreshAcceptance fails a request whose trigger was refused before any run
// started, so it does not sit in queued forever.
func (r *Requests) refreshAcceptance(ctx context.Context, rec *storedRequest, trigger Trigger) bool {
	if trigger == nil || rec.AcceptanceID == "" {
		return false
	}
	reason, rejected, err := trigger.Rejected(ctx, rec.AcceptanceID, rec.Actor)
	if err != nil || !rejected {
		return false
	}
	r.fail(rec, "the customization run was not started: "+reason)
	return true
}

// conclude re-validates a completed run's result in the daemon: the stage's
// verdict is evidence, not authority.
func (r *Requests) conclude(user string, rec *storedRequest, result *CandidateResult) {
	if result == nil {
		r.fail(rec, "the customization run completed without a validated result")
		return
	}
	switch result.Outcome {
	case OutcomeCandidate:
		if result.Candidate == nil {
			r.fail(rec, "the customization run produced no candidate")
			return
		}
		data, err := json.Marshal(result.Candidate)
		if err == nil {
			_, err = Decode(data)
		}
		if err != nil {
			r.fail(rec, "the candidate failed daemon validation: "+err.Error())
			return
		}
		rec.Status, rec.Candidate = StatusReadyToPreview, result.Candidate
	case OutcomeUnsupported:
		label := NormalizeUnmetNeed(result.UnmetNeed)
		if labelEchoesPrompt(label, rec.Prompt) {
			label = UnmetNeedUnlabeled
		}
		rec.Status, rec.UnmetNeed = StatusUnsupported, label
		if label != UnmetNeedUnlabeled {
			// Only the normalized label reaches the aggregate report (#8419).
			_ = r.store.RecordUnsupported(user, label)
		}
	default:
		r.fail(rec, "the customization run completed with invalid output")
		rec.Errors = result.Errors
		return
	}
	rec.UpdatedAt = r.now()
}

// RequestInput is the load-request stage's result file: what the agent needs
// to draft a candidate. The request is nested so the stage's flat outputs,
// which the journal and telemetry index, never carry the prompt.
type RequestInput struct {
	Schema  string         `json:"schema"`
	Attempt int            `json:"attempt"`
	Request RequestText    `json:"request"`
	Current *Customization `json:"current"`
	Surface map[string]any `json:"surface"`
}

// RequestText is the user's request as the agent sees it.
type RequestText struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// InputForRun resolves the request bound to runID with the owner's current
// customization. ErrBindingNotFound means the daemon has not bound the run yet.
func (r *Requests) InputForRun(runID string) (RequestInput, error) {
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return RequestInput{}, ErrBindingNotFound
	}
	data, err := os.ReadFile(r.bindingPath(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return RequestInput{}, ErrBindingNotFound
	}
	if err != nil {
		return RequestInput{}, fmt.Errorf("fluidui: read run binding: %w", err)
	}
	var binding RunBinding
	if err := json.Unmarshal(data, &binding); err != nil || binding.Schema != requestSchema || !userHashPattern.MatchString(binding.User) {
		return RequestInput{}, fmt.Errorf("fluidui: malformed run binding for %s", runID)
	}
	r.mu.Lock()
	file, err := r.load(binding.User)
	r.mu.Unlock()
	if err != nil {
		return RequestInput{}, err
	}
	for _, rec := range file.Requests {
		if rec.ID == binding.RequestID && rec.RunID == runID {
			return r.input(binding, rec)
		}
	}
	return RequestInput{}, ErrBindingNotFound
}

// RedactRunContent removes a bound request's prompt from run content unless
// viewer owns the request. Bindings remain private and durable so completed
// runs and superseded attempts keep the same protection.
func (r *Requests) RedactRunContent(runID, viewer string, content []byte) ([]byte, error) {
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return nil, ErrBindingNotFound
	}
	data, err := os.ReadFile(r.bindingPath(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return content, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fluidui: read run binding: %w", err)
	}
	var binding RunBinding
	if err := json.Unmarshal(data, &binding); err != nil ||
		binding.Schema != requestSchema ||
		!userHashPattern.MatchString(binding.User) {
		return nil, fmt.Errorf("fluidui: malformed run binding for %s", runID)
	}
	if binding.User == userHash(viewer) {
		return content, nil
	}
	prompt, err := r.bindingPrompt(binding)
	if err != nil {
		return nil, fmt.Errorf("fluidui: protect run binding for %s: %w", runID, err)
	}
	return redactPrompt(content, prompt), nil
}

func (r *Requests) bindingPrompt(binding RunBinding) (string, error) {
	if binding.Prompt != "" {
		return binding.Prompt, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := r.load(binding.User)
	if err != nil {
		return "", err
	}
	for _, rec := range file.Requests {
		if rec.ID == binding.RequestID && rec.Prompt != "" {
			return rec.Prompt, nil
		}
	}
	return "", errors.New("request prompt is unavailable")
}

func redactPrompt(content []byte, prompt string) []byte {
	jsonPrompt, _ := json.Marshal(prompt)
	variants := [][]byte{[]byte(prompt)}
	if len(jsonPrompt) >= 2 {
		variants = append(variants, jsonPrompt[1:len(jsonPrompt)-1])
	}
	slices.SortFunc(variants, func(a, b []byte) int { return len(b) - len(a) })
	redacted := content
	for _, variant := range variants {
		if len(variant) != 0 {
			redacted = []byte(strings.ReplaceAll(string(redacted), string(variant), "[REDACTED]"))
		}
	}
	return redacted
}

func (r *Requests) input(binding RunBinding, rec storedRequest) (RequestInput, error) {
	current, err := r.store.loadPath(r.store.hashPath(binding.User))
	if err != nil {
		return RequestInput{}, err
	}
	return RequestInput{
		Schema:  InputSchema,
		Attempt: binding.Attempt,
		Request: RequestText{ID: rec.ID, Text: rec.Prompt},
		Current: current.state().Customization,
		Surface: Surface(),
	}, nil
}
