package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
)

const maxInterventionBody = 1 << 20

// InterventionRequest is the transport-neutral input shared by the CLI and
// dashboard mutation adapters. RunID and Stage come from the route path.
type InterventionRequest struct {
	RunID               string `json:"-"`
	Stage               string `json:"-"`
	IdempotencyKey      string `json:"-"`
	Actor               string `json:"actor,omitempty"`
	Decision            string `json:"decision,omitempty"`
	Rationale           string `json:"rationale,omitempty"`
	InstructionAddendum string `json:"instructionAddendum,omitempty"`
}

// InterventionResult reports the durable run position after an action.
type InterventionResult struct {
	Phase      string `json:"phase"`
	State      string `json:"state,omitempty"`
	JournalSeq uint64 `json:"journalSeq"`
}

// InterventionService separates bounded request admission from daemon-owned
// execution. Methods return only after the action has a durable handoff.
type InterventionService interface {
	AcceptApprove(admission, execution context.Context, input InterventionRequest) (InterventionResult, error)
	AcceptOverride(admission, execution context.Context, input InterventionRequest) (InterventionResult, error)
	AcceptRerunStage(admission, execution context.Context, input InterventionRequest) (InterventionResult, error)
}

// InterventionError is a safe, typed action refusal returned to API clients.
type InterventionError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *InterventionError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Message
}

func (e *InterventionError) Unwrap() error { return e.Err }

// NewInterventionError constructs a safe action error for an adapter service.
func NewInterventionError(status int, code, message string, err error) error {
	return &InterventionError{Status: status, Code: code, Message: message, Err: err}
}

func registerMutationRoutes(router *Router, interventions InterventionService, lifecycle context.Context, errorLog *log.Logger) {
	router.Handle(apicontract.RouteApproveStage, stageMutationHandler("approve", interventions, lifecycle, errorLog))
	router.Handle(apicontract.RouteOverrideStage, stageMutationHandler("override", interventions, lifecycle, errorLog))
	router.Handle(apicontract.RouteRerunStage, stageMutationHandler("rerun", interventions, lifecycle, errorLog))
}

func stageMutationHandler(action string, interventions InterventionService, lifecycle context.Context, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if interventions == nil {
			writeError(w, http.StatusServiceUnavailable, "interventions_unavailable", "run interventions are not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		key, ok := requireIdempotencyKey(w, request)
		if !ok {
			return
		}
		input, err := decodeInterventionRequest(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok {
			input.Actor = principal.Subject
		}
		if strings.TrimSpace(input.Actor) == "" {
			writeError(w, http.StatusBadRequest, "actor_required", "actor is required")
			return
		}
		input.IdempotencyKey = key
		var result InterventionResult
		switch action {
		case "approve":
			result, err = interventions.AcceptApprove(request.Context(), lifecycle, input)
		case "override":
			result, err = interventions.AcceptOverride(request.Context(), lifecycle, input)
		case "rerun":
			result, err = interventions.AcceptRerunStage(request.Context(), lifecycle, input)
		default:
			panic("unknown intervention action " + action)
		}
		if err != nil {
			if budgetExceeded(w, err) {
				return
			}
			var interventionErr *InterventionError
			if errors.As(err, &interventionErr) {
				status := interventionErr.Status
				if status < 400 || status > 599 {
					status = http.StatusInternalServerError
				}
				if status >= http.StatusInternalServerError {
					errorLog.Printf("%s run intervention failed: %v", action, err)
				}
				code := interventionErr.Code
				if code == "" {
					code = "intervention_failed"
				}
				message := interventionErr.Message
				if message == "" {
					message = "run intervention failed"
				}
				writeError(w, status, code, message)
				return
			}
			errorLog.Printf("%s run intervention failed: %v", action, err)
			writeError(w, http.StatusInternalServerError, "intervention_failed", "run intervention failed")
			return
		}
		if result.JournalSeq == 0 {
			errorLog.Printf("%s run intervention returned no journal position", action)
			writeError(w, http.StatusInternalServerError, "intervention_failed", "run intervention returned no journal position")
			return
		}
		w.Header().Set(HeaderSourceApplied, fmt.Sprintf("%s:%d", input.RunID, result.JournalSeq))
		writeJSON(w, http.StatusOK, result)
	}
}

func validateMutationTransport(request *http.Request) (int, string, string) {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"
	}
	if origin := strings.TrimSpace(request.Header.Get("Origin")); origin != "" {
		parsed, parseErr := url.Parse(origin)
		if parseErr != nil ||
			parsed.User != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" ||
			parsed.Path != "" ||
			parsed.RawQuery != "" ||
			parsed.Fragment != "" ||
			!isLoopbackAuthority(parsed.Host) ||
			!isLoopbackAuthority(request.Host) ||
			!strings.EqualFold(parsed.Host, request.Host) {
			return http.StatusForbidden, "origin_forbidden", "cross-origin mutation requests are forbidden"
		}
	}
	return 0, "", ""
}

func isLoopbackAuthority(authority string) bool {
	host := authority
	if parsedHost, _, err := net.SplitHostPort(authority); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func decodeInterventionRequest(request *http.Request) (InterventionRequest, error) {
	defer func() { _ = request.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxInterventionBody))
	decoder.DisallowUnknownFields()
	var input InterventionRequest
	if err := decoder.Decode(&input); err != nil {
		if errors.Is(err, io.EOF) {
			return InterventionRequest{}, errors.New("JSON request body is required")
		}
		return InterventionRequest{}, fmt.Errorf("invalid JSON request body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return InterventionRequest{}, errors.New("request body must contain one JSON object")
		}
		return InterventionRequest{}, fmt.Errorf("invalid JSON request body: %w", err)
	}
	input.RunID = request.PathValue("run")
	input.Stage = request.PathValue("stage")
	return input, nil
}
