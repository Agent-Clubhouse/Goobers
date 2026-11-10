package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
)

// LabsFluidUIRequestService keeps each user's natural-language customization
// requests (#8417). user is always the caller's own fluidui.UserKey.
type LabsFluidUIRequestService interface {
	Submit(ctx context.Context, user, actor, prompt string, trigger fluidui.Trigger) (fluidui.Request, error)
	List(ctx context.Context, user string, trigger fluidui.Trigger) ([]fluidui.Request, error)
	Get(ctx context.Context, user, id string, trigger fluidui.Trigger) (fluidui.Request, error)
	Retry(ctx context.Context, user, id string, trigger fluidui.Trigger) (fluidui.Request, error)
	Apply(ctx context.Context, user, id string) (fluidui.Request, fluidui.State, error)
	Revert(ctx context.Context, user, id string) (fluidui.Request, fluidui.State, error)
}

// WithLabsFluidUIRequests enables the Labs Fluid UI request routes. Submit and
// retry also need the daemon's trigger plane (WithTriggerService).
func WithLabsFluidUIRequests(service LabsFluidUIRequestService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API labs fluid UI request service is required")
		}
		config.labsFluidUIRequests = service
		return nil
	}
}

type labsFluidUIRequestList struct {
	Requests []fluidui.Request `json:"requests"`
}

type labsFluidUIApplication struct {
	Request       fluidui.Request `json:"request"`
	Customization fluidui.State   `json:"customization"`
}

// labsFluidUITrigger starts the customization workflow through the same
// trigger ingest a manual start uses, attributed to the requesting user.
type labsFluidUITrigger struct{ triggers TriggerService }

func (t labsFluidUITrigger) Start(ctx context.Context, key, actor string) (string, string, error) {
	response, err := t.triggers.Trigger(ctx, TriggerRequest{Workflow: fluidui.Workflow, RequestID: key, Actor: actor})
	if err != nil {
		return "", "", err
	}
	if response.AcceptanceID == "" && response.RunID == "" {
		return "", "", errors.New("the trigger was not accepted (" + response.State + ")")
	}
	return response.AcceptanceID, response.RunID, nil
}

func (t labsFluidUITrigger) Rejected(ctx context.Context, acceptanceID, actor string) (string, bool, error) {
	status, ok := t.triggers.(TriggerStatusService)
	if !ok {
		return "", false, nil
	}
	response, err := status.TriggerStatus(ctx, TriggerStatusRequest{AcceptanceID: acceptanceID, Actor: actor})
	if err != nil {
		return "", false, err
	}
	return response.Reason, response.State == "rejected", nil
}

func labsFluidUIRequestTrigger(triggers TriggerService) fluidui.Trigger {
	if triggers == nil {
		return nil
	}
	return labsFluidUITrigger{triggers: triggers}
}

func labsFluidUIActor(request *http.Request) string {
	if principal, ok := PrincipalFromRequest(request); ok {
		return principal.Subject
	}
	return ""
}

func registerLabsFluidUIRequestRoutes(router *Router, service LabsFluidUIRequestService, triggers TriggerService, errorLog *log.Logger) {
	trigger := labsFluidUIRequestTrigger(triggers)
	handle := func(write bool, status int, operation func(*http.Request, string) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if service == nil {
				writeError(w, http.StatusServiceUnavailable, "labs_unavailable", "Labs Fluid UI requests are not configured on this daemon")
				return
			}
			if write {
				if code, errCode, message := validateMutationTransport(request); code != 0 {
					writeError(w, code, errCode, message)
					return
				}
			}
			body, err := operation(request, labsFluidUIUser(request))
			if err != nil {
				writeLabsFluidUIRequestError(w, errorLog, err)
				return
			}
			writeJSON(w, status, body)
		}
	}
	router.HandleByMethod(
		map[string]apicontract.RouteID{
			http.MethodGet:  apicontract.RouteLabsFluidUIRequestsList,
			http.MethodPost: apicontract.RouteLabsFluidUIRequestSubmit,
		},
		map[apicontract.RouteID]http.HandlerFunc{
			apicontract.RouteLabsFluidUIRequestsList: handle(false, http.StatusOK, func(request *http.Request, user string) (any, error) {
				requests, err := service.List(request.Context(), user, trigger)
				return labsFluidUIRequestList{Requests: requests}, err
			}),
			apicontract.RouteLabsFluidUIRequestSubmit: handle(true, http.StatusAccepted, func(request *http.Request, user string) (any, error) {
				prompt, err := decodeLabsFluidUIPrompt(request)
				if err != nil {
					return nil, err
				}
				return service.Submit(request.Context(), user, labsFluidUIActor(request), prompt, trigger)
			}),
		},
	)
	router.Handle(apicontract.RouteLabsFluidUIRequestGet, handle(false, http.StatusOK, func(request *http.Request, user string) (any, error) {
		return service.Get(request.Context(), user, request.PathValue("request"), trigger)
	}))
	router.Handle(apicontract.RouteLabsFluidUIRequestRetry, handle(true, http.StatusAccepted, func(request *http.Request, user string) (any, error) {
		if err := requireLabsFluidUIEmptyBody(request); err != nil {
			return nil, err
		}
		return service.Retry(request.Context(), user, request.PathValue("request"), trigger)
	}))
	application := func(revert bool) func(*http.Request, string) (any, error) {
		return func(request *http.Request, user string) (any, error) {
			if err := requireLabsFluidUIEmptyBody(request); err != nil {
				return nil, err
			}
			apply := service.Apply
			if revert {
				apply = service.Revert
			}
			result, state, err := apply(request.Context(), user, request.PathValue("request"))
			return labsFluidUIApplication{Request: result, Customization: state}, err
		}
	}
	router.Handle(apicontract.RouteLabsFluidUIRequestApply, handle(true, http.StatusOK, application(false)))
	router.Handle(apicontract.RouteLabsFluidUIRequestRevert, handle(true, http.StatusOK, application(true)))
}

func decodeLabsFluidUIPrompt(request *http.Request) (string, error) {
	data, err := io.ReadAll(io.LimitReader(request.Body, 16<<10))
	if err != nil {
		return "", &fluidui.ValidationError{Field: "$", Message: "request body could not be read"}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		return "", &fluidui.ValidationError{Field: "$", Message: `request body must be {"prompt": "..."}`}
	}
	return body.Prompt, nil
}

func requireLabsFluidUIEmptyBody(request *http.Request) error {
	data, err := io.ReadAll(io.LimitReader(request.Body, 64))
	if err != nil {
		return &fluidui.ValidationError{Field: "$", Message: "request body could not be read"}
	}
	if body := strings.TrimSpace(string(data)); body != "" && body != "{}" {
		return &fluidui.ValidationError{Field: "$", Message: "request body must be empty or {}"}
	}
	return nil
}

func writeLabsFluidUIRequestError(w http.ResponseWriter, errorLog *log.Logger, err error) {
	switch {
	case errors.Is(err, fluidui.ErrRequestNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such request")
	case errors.Is(err, fluidui.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", "the request is not in a state that allows this action")
	case errors.Is(err, fluidui.ErrRevertStale):
		writeError(w, http.StatusConflict, "revert_stale", "the customization changed after this request was applied; undo or reset it instead")
	case errors.Is(err, fluidui.ErrTooManyRequests):
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "too many customization requests are in flight; wait for one to finish")
	case errors.Is(err, fluidui.ErrTriggerUnavailable):
		writeError(w, http.StatusServiceUnavailable, "triggers_unavailable", "this server cannot start the customization workflow")
	default:
		writeLabsFluidUIError(w, errorLog, err)
	}
}
