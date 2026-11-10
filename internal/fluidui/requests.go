package fluidui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/platform/durability"
)

// Workflow is the shipped workflow that turns a request into a candidate.
const Workflow = "fluid-ui-customize"

// Status is where a natural-language customization request stands (#8417).
// Every in-flight status is derived from the run, never assumed.
type Status string

// Request lifecycle.
const (
	StatusQueued         Status = "queued"
	StatusImplementing   Status = "implementing"
	StatusReadyToPreview Status = "ready_to_preview"
	StatusApplied        Status = "applied"
	StatusFailed         Status = "failed"
	StatusUnsupported    Status = "unsupported"
	StatusReverted       Status = "reverted"
)

// Request bounds.
const (
	MaxPromptBytes     = 2000
	MaxRequestsPerUser = 50
	MaxActivePerUser   = 3
	MaxAttempts        = 5
	maxDetailLen       = 512
	requestSchema      = 1
)

// Request errors the API maps to client responses.
var (
	ErrRequestNotFound    = errors.New("fluidui: request not found")
	ErrInvalidTransition  = errors.New("fluidui: request is not in a state that allows this action")
	ErrTooManyRequests    = errors.New("fluidui: too many requests in flight")
	ErrTriggerUnavailable = errors.New("fluidui: the customization workflow cannot be started here")
	ErrRevertStale        = errors.New("fluidui: the customization changed after this request was applied")
	ErrBindingNotFound    = errors.New("fluidui: no request is bound to this run")
)

var (
	requestIDPattern = regexp.MustCompile(`^fr-[0-9a-f]{16}$`)
	userHashPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Request is one user's natural-language customization request. Only its
// owner ever sees it; the prompt never leaves the owner's record except as
// the input of the run that serves it.
type Request struct {
	ID              string         `json:"id"`
	Prompt          string         `json:"prompt"`
	Status          Status         `json:"status"`
	Attempt         int            `json:"attempt"`
	RunID           string         `json:"runId,omitempty"`
	Candidate       *Customization `json:"candidate,omitempty"`
	UnmetNeed       string         `json:"unmetNeed,omitempty"`
	Errors          []string       `json:"errors,omitempty"`
	Detail          string         `json:"detail,omitempty"`
	AppliedRevision int64          `json:"appliedRevision,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

type storedRequest struct {
	Request
	Actor        string `json:"actor,omitempty"`
	AcceptanceID string `json:"acceptanceId,omitempty"`
}

type requestFile struct {
	Schema   int             `json:"schema"`
	Requests []storedRequest `json:"requests"`
}

// RunBinding maps a run to the request it serves. It names the owner only by
// the hashed key the stores already use.
type RunBinding struct {
	Schema    int    `json:"schema"`
	User      string `json:"user"`
	RequestID string `json:"requestId"`
	Attempt   int    `json:"attempt"`
	Prompt    string `json:"prompt"`
}

// Trigger starts the customization workflow through the daemon's trigger
// ingest and reports an acceptance that never became a run.
type Trigger interface {
	Start(ctx context.Context, key, actor string) (acceptanceID, runID string, err error)
	Rejected(ctx context.Context, acceptanceID, actor string) (reason string, rejected bool, err error)
}

// Requests keeps each user's requests beside their customization record and
// derives in-flight status from the run that serves each request.
type Requests struct {
	store *Store
	runs  RunReader
	Now   func() time.Time
	mu    sync.Mutex
}

// NewRequests keeps requests next to store. runs may be nil where no run can
// be observed, such as inside the workflow's own stage.
func NewRequests(store *Store, runs RunReader) *Requests {
	return &Requests{store: store, runs: runs}
}

func (r *Requests) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Requests) requestPath(hash string) string {
	return filepath.Join(r.store.Dir, "requests", hash+".json")
}

func (r *Requests) bindingPath(runID string) string {
	return filepath.Join(r.store.Dir, "runs", runID+".json")
}

// Submit records a new request and starts the workflow for it.
func (r *Requests) Submit(ctx context.Context, user, actor, prompt string, trigger Trigger) (Request, error) {
	prompt = strings.TrimSpace(prompt)
	if err := validatePrompt(prompt); err != nil {
		return Request{}, err
	}
	if trigger == nil {
		return Request{}, ErrTriggerUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	hash := userHash(user)
	file, err := r.load(hash)
	if err != nil {
		return Request{}, err
	}
	r.refreshAll(ctx, user, &file, trigger)
	if err := admit(&file); err != nil {
		return Request{}, err
	}
	now := r.now()
	rec := storedRequest{Request: Request{ID: newRequestID(), Prompt: prompt, CreatedAt: now}, Actor: actor}
	r.start(ctx, hash, &rec, trigger)
	file.Requests = append(file.Requests, rec)
	if err := r.save(hash, file); err != nil {
		return Request{}, err
	}
	return rec.Request, nil
}

// List returns the user's requests, newest first, with in-flight status
// refreshed from their runs.
func (r *Requests) List(ctx context.Context, user string, trigger Trigger) ([]Request, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hash := userHash(user)
	file, err := r.load(hash)
	if err != nil {
		return nil, err
	}
	if r.refreshAll(ctx, user, &file, trigger) {
		if err := r.save(hash, file); err != nil {
			return nil, err
		}
	}
	out := make([]Request, 0, len(file.Requests))
	for i := len(file.Requests) - 1; i >= 0; i-- {
		out = append(out, file.Requests[i].Request)
	}
	return out, nil
}

// Get returns one of the user's requests. Another user's request is
// indistinguishable from one that does not exist.
func (r *Requests) Get(ctx context.Context, user, id string, trigger Trigger) (Request, error) {
	return r.mutate(ctx, user, id, trigger, func(*storedRequest) (bool, error) { return false, nil })
}

// Retry starts a fresh attempt for a failed or unsupported request.
func (r *Requests) Retry(ctx context.Context, user, id string, trigger Trigger) (Request, error) {
	if trigger == nil {
		return Request{}, ErrTriggerUnavailable
	}
	return r.mutate(ctx, user, id, trigger, func(rec *storedRequest) (bool, error) {
		if rec.Status != StatusFailed && rec.Status != StatusUnsupported || rec.Attempt >= MaxAttempts {
			return false, ErrInvalidTransition
		}
		r.start(ctx, userHash(user), rec, trigger)
		return true, nil
	})
}

// Apply makes a ready_to_preview candidate the user's customization through
// the same validated store write the editor uses. Nothing applies a
// candidate except this explicit call.
func (r *Requests) Apply(ctx context.Context, user, id string) (Request, State, error) {
	var state State
	request, err := r.mutate(ctx, user, id, nil, func(rec *storedRequest) (bool, error) {
		if rec.Status != StatusReadyToPreview || rec.Candidate == nil {
			return false, ErrInvalidTransition
		}
		applied, err := r.store.Put(user, *rec.Candidate)
		if err != nil {
			return false, err
		}
		state = applied
		rec.Status, rec.AppliedRevision = StatusApplied, applied.Revision
		return true, nil
	})
	return request, state, err
}

// Revert undoes an applied request, but only while its revision is still the
// current one, so it never discards a later change.
func (r *Requests) Revert(ctx context.Context, user, id string) (Request, State, error) {
	var state State
	request, err := r.mutate(ctx, user, id, nil, func(rec *storedRequest) (bool, error) {
		if rec.Status != StatusApplied {
			return false, ErrInvalidTransition
		}
		current, err := r.store.Get(user)
		if err != nil {
			return false, err
		}
		if current.Revision != rec.AppliedRevision {
			return false, ErrRevertStale
		}
		restored, err := r.store.Undo(user)
		if errors.Is(err, ErrNothingToUndo) {
			restored, err = r.store.Reset(user)
		}
		if err != nil {
			return false, err
		}
		state = restored
		rec.Status = StatusReverted
		return true, nil
	})
	return request, state, err
}

func (r *Requests) mutate(ctx context.Context, user, id string, trigger Trigger, change func(*storedRequest) (bool, error)) (Request, error) {
	if !requestIDPattern.MatchString(id) {
		return Request{}, ErrRequestNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	hash := userHash(user)
	file, err := r.load(hash)
	if err != nil {
		return Request{}, err
	}
	index := slices.IndexFunc(file.Requests, func(rec storedRequest) bool { return rec.ID == id })
	if index < 0 {
		return Request{}, ErrRequestNotFound
	}
	rec := &file.Requests[index]
	dirty := r.refresh(ctx, user, rec, trigger)
	changed, err := change(rec)
	if changed {
		rec.UpdatedAt = r.now()
	}
	if dirty || changed {
		if saveErr := r.save(hash, file); saveErr != nil && err == nil {
			err = saveErr
		}
	}
	if err != nil {
		return Request{}, err
	}
	return rec.Request, nil
}

// start triggers one attempt. A refused trigger fails the request visibly
// instead of leaving it queued forever.
func (r *Requests) start(ctx context.Context, hash string, rec *storedRequest, trigger Trigger) {
	rec.Attempt++
	rec.Status, rec.RunID, rec.AcceptanceID = StatusQueued, "", ""
	rec.Candidate, rec.UnmetNeed, rec.Errors, rec.Detail, rec.AppliedRevision = nil, "", nil, "", 0
	rec.UpdatedAt = r.now()
	key := fmt.Sprintf("fluid-ui/%s/%s/%d", hash, rec.ID, rec.Attempt)
	acceptanceID, runID, err := trigger.Start(ctx, key, rec.Actor)
	if err == nil && runID == "" {
		runID = strings.TrimPrefix(acceptanceID, "trigger-")
	}
	if err == nil {
		err = r.bind(hash, rec, runID)
	}
	if err != nil {
		r.fail(rec, "the customization workflow could not be started: "+err.Error())
		return
	}
	rec.AcceptanceID, rec.RunID = acceptanceID, runID
}

func (r *Requests) bind(hash string, rec *storedRequest, runID string) error {
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return errors.New("the trigger returned no usable run")
	}
	data, err := json.Marshal(RunBinding{Schema: requestSchema, User: hash, RequestID: rec.ID, Attempt: rec.Attempt, Prompt: rec.Prompt})
	if err != nil {
		return err
	}
	path := r.bindingPath(runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return durability.WriteFileAtomic(path, data, 0o600)
}

func (r *Requests) fail(rec *storedRequest, detail string) {
	if len(detail) > maxDetailLen {
		detail = detail[:maxDetailLen]
	}
	rec.Status, rec.Detail, rec.UpdatedAt = StatusFailed, detail, r.now()
}

func admit(file *requestFile) error {
	active := 0
	for _, rec := range file.Requests {
		if rec.Status == StatusQueued || rec.Status == StatusImplementing {
			active++
		}
	}
	if active >= MaxActivePerUser {
		return ErrTooManyRequests
	}
	if len(file.Requests) < MaxRequestsPerUser {
		return nil
	}
	for i, rec := range file.Requests {
		switch rec.Status {
		case StatusFailed, StatusUnsupported, StatusReverted, StatusApplied:
			file.Requests = slices.Delete(file.Requests, i, i+1)
			return nil
		}
	}
	return ErrTooManyRequests
}

func validatePrompt(prompt string) error {
	if prompt == "" {
		return invalid("prompt", "describe the change you want")
	}
	if len(prompt) > MaxPromptBytes {
		return invalid("prompt", "at most %d bytes", MaxPromptBytes)
	}
	if !utf8.ValidString(prompt) {
		return invalid("prompt", "must be valid UTF-8")
	}
	for _, c := range prompt {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return invalid("prompt", "contains a control character")
		}
	}
	return nil
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "fr-" + hex.EncodeToString(b)
}

func (r *Requests) load(hash string) (requestFile, error) {
	data, err := os.ReadFile(r.requestPath(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return requestFile{Schema: requestSchema}, nil
	}
	if err != nil {
		return requestFile{}, fmt.Errorf("fluidui: read requests: %w", err)
	}
	var file requestFile
	if err := json.Unmarshal(data, &file); err != nil {
		return requestFile{}, fmt.Errorf("fluidui: decode requests: %w", err)
	}
	if file.Schema != requestSchema {
		return requestFile{}, fmt.Errorf("fluidui: unsupported requests schema %d", file.Schema)
	}
	return file, nil
}

func (r *Requests) save(hash string, file requestFile) error {
	file.Schema = requestSchema
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("fluidui: encode requests: %w", err)
	}
	path := r.requestPath(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("fluidui: create request store: %w", err)
	}
	if err := durability.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("fluidui: write requests: %w", err)
	}
	return nil
}
