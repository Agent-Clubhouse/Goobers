package fluidui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	r.unbind(rec.RunID)
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
	r.unbind(rec.RunID)
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

func (r *Requests) unbind(runID string) {
	if runID != "" {
		_ = os.Remove(r.bindingPath(runID))
	}
}

// RequestInput is the load-request stage's result file: what the agent needs
// to draft a candidate. It never carries the prompt: the result is a run
// artifact any reader of the run can open, so the prompt reaches the agent only
// as a private harness input (PromptForRun, #8425).
type RequestInput struct {
	Schema  string         `json:"schema"`
	Attempt int            `json:"attempt"`
	Request RequestRef     `json:"request"`
	Current *Customization `json:"current"`
	Surface map[string]any `json:"surface"`
}

// RequestRef identifies the request a run serves.
type RequestRef struct {
	ID string `json:"id"`
}

// PrivatePromptFile is the private input name the request text reaches the
// agent under.
const PrivatePromptFile = "fluid-ui-request-text.txt"

// InputForRun resolves the request bound to runID with the owner's current
// customization. ErrBindingNotFound means the daemon has not bound the run yet.
func (r *Requests) InputForRun(runID string) (RequestInput, error) {
	binding, rec, err := r.boundRequest(runID)
	if err != nil {
		return RequestInput{}, err
	}
	return r.input(binding, rec)
}

// PromptForRun returns the text of the request bound to runID, for the
// harness to hand the agent privately. ErrBindingNotFound means no request is
// bound to the run.
func (r *Requests) PromptForRun(runID string) (string, error) {
	_, rec, err := r.boundRequest(runID)
	if err != nil {
		return "", err
	}
	return rec.Prompt, nil
}

// SensitivePromptValues lists the exact values a run's journal must redact to
// keep prompt out of it: the whole prompt and each line long enough to
// register, since an agent tool may show a multi-line prompt line by line.
func SensitivePromptValues(prompt string) [][]byte {
	values := [][]byte{[]byte(prompt)}
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= MinPromptBytes && line != prompt {
			values = append(values, []byte(line))
		}
	}
	return values
}

func (r *Requests) boundRequest(runID string) (RunBinding, storedRequest, error) {
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return RunBinding{}, storedRequest{}, ErrBindingNotFound
	}
	data, err := os.ReadFile(r.bindingPath(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return RunBinding{}, storedRequest{}, ErrBindingNotFound
	}
	if err != nil {
		return RunBinding{}, storedRequest{}, fmt.Errorf("fluidui: read run binding: %w", err)
	}
	var binding RunBinding
	if err := json.Unmarshal(data, &binding); err != nil || binding.Schema != requestSchema || !userHashPattern.MatchString(binding.User) {
		return RunBinding{}, storedRequest{}, fmt.Errorf("fluidui: malformed run binding for %s", runID)
	}
	r.mu.Lock()
	file, err := r.load(binding.User)
	r.mu.Unlock()
	if err != nil {
		return RunBinding{}, storedRequest{}, err
	}
	for _, rec := range file.Requests {
		if rec.ID == binding.RequestID && rec.RunID == runID {
			return binding, rec, nil
		}
	}
	return RunBinding{}, storedRequest{}, ErrBindingNotFound
}

func (r *Requests) input(binding RunBinding, rec storedRequest) (RequestInput, error) {
	current, err := r.store.loadPath(r.store.hashPath(binding.User))
	if err != nil {
		return RequestInput{}, err
	}
	return RequestInput{
		Schema:  InputSchema,
		Attempt: binding.Attempt,
		Request: RequestRef{ID: rec.ID},
		Current: current.state().Customization,
		Surface: Surface(),
	}, nil
}
