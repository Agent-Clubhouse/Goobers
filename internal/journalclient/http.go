package journalclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
)

// Error is a typed refusal from the daemon: the shared API error envelope's
// code and message beside the HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("journal plane refused (%d %s): %s", e.Status, e.Code, e.Message)
}

// DefaultHTTPTimeout bounds one round trip. The daemon's own budget on the
// same-run read routes is 8s (apicontract.BoundedBudget) and 60s on the
// artifact route (BlobBudget); this contains the larger of the two plus
// margin, so a client timeout is never the first thing to fire.
const DefaultHTTPTimeout = 90 * time.Second

// MaxEventListBytes bounds one Events() response. A run journal's scrubbed
// projection is bounded by the run, not by history, but a bound must exist:
// an unbounded read of a hostile response is how a stage pod runs out of
// memory.
const MaxEventListBytes = 64 << 20

// MaxArtifactBytes bounds one artifact fetch, matching the journal plane's
// own inline-artifact ceiling class.
const MaxArtifactBytes = 64 << 20

// maxErrorBodyBytes bounds an error envelope read.
const maxErrorBodyBytes = 4 << 20

// HTTPConfig configures the run-read backend.
type HTTPConfig struct {
	// BaseURL is the daemon API root (EnvEndpoint in the pod).
	BaseURL string
	// Token is the journal-scoped bearer (EnvToken in the pod).
	Token string
	// RunID is the stage's own run — the route's containment key. Every
	// same-run read addresses this run and no other; the client refuses to
	// build a request for a different one rather than relying on the server
	// to refuse it.
	RunID string
	// Gaggle scopes the cross-run routes.
	Gaggle string
	// Client overrides the HTTP client; nil uses a bounded default.
	Client *http.Client
}

// HTTP is the daemon-backed backend: Reader over the run-scoped read routes,
// CrossRun over the gaggle-scoped journal plane.
type HTTP struct {
	cfg HTTPConfig
}

// NewHTTP constructs the plane backend, refusing an incomplete configuration
// rather than deferring the refusal to the first call.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("journalclient: HTTP backend requires a base URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("journalclient: HTTP backend requires a bearer token")
	}
	if strings.TrimSpace(cfg.RunID) == "" {
		return nil, errors.New("journalclient: HTTP backend requires the stage's run ID")
	}
	if !apiv1.ValidRunID(cfg.RunID) {
		return nil, fmt.Errorf("journalclient: %q is not a valid run id", cfg.RunID)
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	return &HTTP{cfg: cfg}, nil
}

// NewHTTPFromSelection builds the backend from a Select result.
func NewHTTPFromSelection(selection Selection) (*HTTP, error) {
	return NewHTTP(HTTPConfig{
		BaseURL: selection.Endpoint,
		Token:   selection.Token,
		RunID:   selection.RunID,
		Gaggle:  selection.Gaggle,
	})
}

// RunID implements Reader.
func (h *HTTP) RunID() string { return h.cfg.RunID }

func (h *HTTP) do(ctx context.Context, method, path string, body any, limit int64) ([]byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("journalclient: encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	endpoint := h.cfg.BaseURL + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("journalclient: build request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("journalclient: %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
		return nil, nil, planeError(response.StatusCode, raw)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return nil, nil, fmt.Errorf("journalclient: read response from %s: %w", endpoint, err)
	}
	if int64(len(raw)) >= limit {
		return nil, nil, fmt.Errorf("journalclient: response from %s exceeds the %d byte ceiling", endpoint, limit)
	}
	return raw, response.Header, nil
}

func planeError(status int, raw []byte) error {
	planeErr := &Error{Status: status}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
		planeErr.Code, planeErr.Message = envelope.Error.Code, envelope.Error.Message
	} else {
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 400 {
			detail = detail[:400] + "…"
		}
		planeErr.Code, planeErr.Message = "http_"+fmt.Sprint(status), detail
	}
	return planeErr
}

func (h *HTTP) getJSON(ctx context.Context, path string, limit int64, target any) error {
	raw, _, err := h.do(ctx, http.MethodGet, path, nil, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("journalclient: decode response from %s: %w", path, err)
	}
	return nil
}

func (h *HTTP) post(ctx context.Context, path string, body, target any) error {
	raw, _, err := h.do(ctx, http.MethodPost, path, body, MaxEventListBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("journalclient: decode response from %s: %w", path, err)
	}
	return nil
}

// runPath substitutes this client's own run into a contract path template.
// The run is never taken from a caller: every same-run read is contained to
// the token's run on the client side too, so a bug cannot even ATTEMPT a read
// the daemon would have to refuse.
func (h *HTTP) runPath(template string, extra map[string]string) string {
	path := strings.ReplaceAll(template, "{run}", url.PathEscape(h.cfg.RunID))
	for key, value := range extra {
		path = strings.ReplaceAll(path, "{"+key+"}", url.PathEscape(value))
	}
	return path
}

// --- same-run reads ---------------------------------------------------------

// wireEvent restates the daemon's scrubbed run-event projection
// (readservice.RunEvent), tag for tag, with the fields a stage consumer acts
// on. Restated rather than imported so the stage-side client depends on the
// contract's shape, not on the daemon's read service; the server's tests pin
// the two together.
type wireEvent struct {
	Schema              string                  `json:"schema"`
	Seq                 uint64                  `json:"seq"`
	Type                journal.EventType       `json:"type"`
	Branch              int                     `json:"branch"`
	Time                time.Time               `json:"time"`
	KnownSchema         bool                    `json:"knownSchema"`
	Stage               string                  `json:"stage,omitempty"`
	Attempt             int                     `json:"attempt,omitempty"`
	AttemptClass        string                  `json:"attemptClass,omitempty"`
	Gate                string                  `json:"gate,omitempty"`
	Verdict             string                  `json:"verdict,omitempty"`
	Target              string                  `json:"target,omitempty"`
	Escalated           bool                    `json:"escalated,omitempty"`
	Status              string                  `json:"status,omitempty"`
	Actor               string                  `json:"actor,omitempty"`
	Action              string                  `json:"action,omitempty"`
	Decision            string                  `json:"decision,omitempty"`
	Rationale           string                  `json:"rationale,omitempty"`
	Complete            bool                    `json:"complete,omitempty"`
	InstructionAddendum string                  `json:"instructionAddendum,omitempty"`
	WorkflowVersion     int                     `json:"workflowVersion,omitempty"`
	WorkflowDigest      string                  `json:"workflowDigest,omitempty"`
	Outputs             map[string]any          `json:"outputs,omitempty"`
	Artifacts           []ArtifactMetadata      `json:"artifacts,omitempty"`
	Artifact            *ArtifactMetadata       `json:"artifact,omitempty"`
	Name                string                  `json:"name,omitempty"`
	ExternalRef         *journal.ExternalRef    `json:"externalRef,omitempty"`
	Error               *journal.ErrorDetail    `json:"error,omitempty"`
	Redaction           *journal.RedactionInfo  `json:"redaction,omitempty"`
	Runner              map[string]any          `json:"runner,omitempty"`
	Parallel            string                  `json:"parallel,omitempty"`
	BranchName          string                  `json:"branchName,omitempty"`
	BranchStatus        journal.BranchStatus    `json:"branchStatus,omitempty"`
	Completeness        []journal.BranchOutcome `json:"completeness,omitempty"`
}

type wireEventList struct {
	RunID  string      `json:"runId"`
	Events []wireEvent `json:"events"`
}

// JournalEvent rebuilds the journal.Event a stage consumer switches on.
//
// The one deliberate difference from the on-disk event: Ref and Artifacts
// carry the CANONICAL content-addressed path derived from each digest, not
// the path the writer recorded. The projection omits journal-relative paths
// on purpose (a stage must not learn or steer the daemon's filesystem
// layout), and every read this client serves is digest-addressed anyway.
func (e wireEvent) JournalEvent() journal.Event {
	event := journal.Event{
		Schema:              e.Schema,
		Seq:                 e.Seq,
		Type:                e.Type,
		Branch:              e.Branch,
		Time:                e.Time,
		Stage:               e.Stage,
		Attempt:             e.Attempt,
		AttemptClass:        journal.AttemptClass(e.AttemptClass),
		Actor:               e.Actor,
		Action:              e.Action,
		Decision:            e.Decision,
		InstructionAddendum: e.InstructionAddendum,
		Rationale:           e.Rationale,
		Gate:                e.Gate,
		Verdict:             e.Verdict,
		Target:              e.Target,
		Complete:            e.Complete,
		Escalated:           e.Escalated,
		Status:              e.Status,
		WorkflowVersion:     e.WorkflowVersion,
		WorkflowDigest:      e.WorkflowDigest,
		Outputs:             e.Outputs,
		Name:                e.Name,
		ExternalRef:         e.ExternalRef,
		Error:               e.Error,
		Redaction:           e.Redaction,
		Runner:              e.Runner,
		Parallel:            e.Parallel,
		BranchName:          e.BranchName,
		BranchStatus:        e.BranchStatus,
		Completeness:        e.Completeness,
	}
	if e.Artifact != nil {
		if ref, ok := e.Artifact.Ref(); ok {
			event.Ref = &ref
		}
	}
	for _, metadata := range e.Artifacts {
		if ref, ok := metadata.Ref(); ok {
			event.Artifacts = append(event.Artifacts, ref)
		}
	}
	return event
}

// Events implements Reader over GET /runs/{run}/events.
func (h *HTTP) Events() ([]journal.Event, error) { return h.EventsContext(context.Background()) }

// EventsContext is Events with an explicit context.
func (h *HTTP) EventsContext(ctx context.Context) ([]journal.Event, error) {
	var list wireEventList
	if err := h.getJSON(ctx, h.runPath(apicontract.RunEventsPath, nil), MaxEventListBytes, &list); err != nil {
		return nil, err
	}
	if list.RunID != "" && list.RunID != h.cfg.RunID {
		return nil, fmt.Errorf("journalclient: run events for %q answered for run %q", h.cfg.RunID, list.RunID)
	}
	events := make([]journal.Event, 0, len(list.Events))
	for _, wire := range list.Events {
		events = append(events, wire.JournalEvent())
	}
	return events, nil
}

// ArtifactBytes implements Reader. Only ref.Digest addresses the read — a
// journal-relative Path never leaves this process — and the returned bytes are
// verified against that digest before they are handed back, so a compromised
// or buggy daemon cannot substitute content.
func (h *HTTP) ArtifactBytes(ref journal.Ref) ([]byte, error) {
	return h.ArtifactByDigest(ref.Digest)
}

// ArtifactBytesBounded implements Reader: ArtifactBytes with the ceiling
// applied at the TRANSPORT, so an oversized body is refused as it arrives
// rather than after it has been buffered.
func (h *HTTP) ArtifactBytesBounded(ref journal.Ref, maxBytes int64) ([]byte, error) {
	return h.artifactByDigest(context.Background(), ref.Digest, maxBytes)
}

// ArtifactByDigest implements Reader over GET /runs/{run}/artifacts/{digest}.
func (h *HTTP) ArtifactByDigest(digest string) ([]byte, error) {
	return h.ArtifactByDigestContext(context.Background(), digest)
}

// ArtifactByDigestContext is ArtifactByDigest with an explicit context.
func (h *HTTP) ArtifactByDigestContext(ctx context.Context, digest string) ([]byte, error) {
	return h.artifactByDigest(ctx, digest, 0)
}

func (h *HTTP) artifactByDigest(ctx context.Context, digest string, maxBytes int64) ([]byte, error) {
	if strings.TrimSpace(digest) == "" {
		return nil, errors.New("journalclient: artifact digest is required")
	}
	// Reject a malformed digest here rather than sending it: the same check
	// journal.ArtifactByDigest makes before it touches the filesystem.
	if _, err := journal.ArtifactPath(digest); err != nil {
		return nil, err
	}
	limit := int64(MaxArtifactBytes)
	if maxBytes > 0 && maxBytes < limit {
		// do() treats limit as exclusive, so +1 admits exactly maxBytes and
		// refuses the first byte past it.
		limit = maxBytes + 1
	}
	path := h.runPath(apicontract.RunArtifactPath, map[string]string{"digest": digest})
	raw, header, err := h.do(ctx, http.MethodGet, path, nil, limit)
	if err != nil {
		return nil, err
	}
	// The route names the digest it served. When it does, it must be the one
	// asked for: a mismatch is caught by name before the bytes are digested,
	// so the refusal says which artifact was substituted rather than only
	// that the content was wrong.
	if served := strings.TrimSpace(header.Get(apicontract.DigestHeader)); served != "" && served != digest {
		return nil, fmt.Errorf("journalclient: asked for artifact %s and the daemon served %s", digest, served)
	}
	if got := journal.Digest(raw); got != digest {
		return nil, fmt.Errorf("journalclient: digest mismatch for %s: have %s", digest, got)
	}
	return raw, nil
}

type wireAttemptList struct {
	RunID    string         `json:"runId"`
	Stage    string         `json:"stage"`
	Attempts []StageAttempt `json:"attempts"`
}

// StageAttempts implements Reader over GET /runs/{run}/stages/{stage}/attempts.
func (h *HTTP) StageAttempts(stage string) ([]StageAttempt, error) {
	return h.StageAttemptsContext(context.Background(), stage)
}

// StageAttemptsContext is StageAttempts with an explicit context.
func (h *HTTP) StageAttemptsContext(ctx context.Context, stage string) ([]StageAttempt, error) {
	if strings.TrimSpace(stage) == "" {
		return nil, errors.New("journalclient: stage is required")
	}
	var list wireAttemptList
	path := h.runPath(apicontract.StageAttemptsPath, map[string]string{"stage": stage})
	if err := h.getJSON(ctx, path, MaxEventListBytes, &list); err != nil {
		return nil, err
	}
	return list.Attempts, nil
}

// Phase implements Reader by reconstructing the phase from the run's own
// events — the same rule journal.Reader.Phase applies, applied to the same
// durable log, so the two backends cannot disagree about terminality.
func (h *HTTP) Phase() (journal.RunPhase, error) {
	events, err := h.Events()
	if err != nil {
		return "", err
	}
	return journal.PhaseFromEvents(events), nil
}

var _ Reader = (*HTTP)(nil)

// --- cross-run reads --------------------------------------------------------

// ErrGaggleRequired is the fail-closed refusal for a cross-run question with
// no gaggle to scope it: the plane's cross-run routes are gaggle-scoped by
// construction, and an unscoped walk of every run on the instance is exactly
// what decision 005 R1 declined to expose.
var ErrGaggleRequired = errors.New("journalclient: cross-run journal reads over the plane require a gaggle scope")

func (h *HTTP) gaggle(requested string) (string, error) {
	scope := strings.TrimSpace(requested)
	if scope == "" {
		scope = strings.TrimSpace(h.cfg.Gaggle)
	}
	if scope == "" {
		return "", ErrGaggleRequired
	}
	return scope, nil
}

// RunPhase implements CrossRun over POST /journal/run-phase.
func (h *HTTP) RunPhase(ctx context.Context, targetRunID string) (journal.RunPhase, error) {
	if strings.TrimSpace(targetRunID) == "" {
		return "", errors.New("journalclient: target run id is required")
	}
	scope, err := h.gaggle("")
	if err != nil {
		return "", err
	}
	var response RunPhaseResponse
	if err := h.post(ctx, apicontract.JournalRunPhasePath, RunPhaseRequest{
		RunID:       h.cfg.RunID,
		TargetRunID: targetRunID,
		Gaggle:      scope,
	}, &response); err != nil {
		return "", err
	}
	if response.Phase == "" {
		return "", fmt.Errorf("journalclient: run-phase answered no phase for %s", targetRunID)
	}
	return journal.RunPhase(response.Phase), nil
}

// ConflictTouches implements CrossRun over POST /journal/conflict-touches.
func (h *HTTP) ConflictTouches(ctx context.Context, req ConflictTouchRequest) ([]ConflictTouch, error) {
	scope, err := h.gaggle(req.Gaggle)
	if err != nil {
		return nil, err
	}
	var response ConflictTouchResponse
	if err := h.post(ctx, apicontract.JournalConflictTouchesPath, ConflictTouchRequest{
		RunID:  h.cfg.RunID,
		Gaggle: scope,
		Since:  req.Since,
	}, &response); err != nil {
		return nil, err
	}
	if response.Touches == nil {
		return []ConflictTouch{}, nil
	}
	return response.Touches, nil
}

// EscalationCandidates implements CrossRun over POST /journal/escalation-candidates.
func (h *HTTP) EscalationCandidates(ctx context.Context, req EscalationCandidatesRequest) ([]EscalationCandidate, error) {
	scope, err := h.gaggle(req.Gaggle)
	if err != nil {
		return nil, err
	}
	var response EscalationCandidatesResponse
	if err := h.post(ctx, apicontract.JournalEscalationCandidatesPath, EscalationCandidatesRequest{
		RunID:  h.cfg.RunID,
		Gaggle: scope,
	}, &response); err != nil {
		return nil, err
	}
	if response.Candidates == nil {
		return []EscalationCandidate{}, nil
	}
	return response.Candidates, nil
}

// UnpushedWork implements CrossRun over POST /journal/unpushed-work.
//
// The item list rides along for diagnostics only: the daemon derives the
// asking run's claimed items from its own ledger and answers about those,
// so a pod cannot ask about work it does not hold.
func (h *HTTP) UnpushedWork(ctx context.Context, req UnpushedWorkRequest) (*UnpushedWork, error) {
	scope, err := h.gaggle(req.Gaggle)
	if err != nil {
		return nil, err
	}
	var response UnpushedWorkResponse
	if err := h.post(ctx, apicontract.JournalUnpushedWorkPath, UnpushedWorkRequest{
		RunID:              h.cfg.RunID,
		Gaggle:             scope,
		Since:              req.Since,
		ItemIDs:            req.ItemIDs,
		MaxInlineDiffBytes: req.MaxInlineDiffBytes,
	}, &response); err != nil {
		return nil, err
	}
	return response.Work, nil
}

var _ CrossRun = (*HTTP)(nil)
