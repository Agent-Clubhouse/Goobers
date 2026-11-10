package httpapi

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fluidui"
)

// LabsFluidUIService stores each user's Portal customization (#8415). Every
// method is keyed by fluidui.UserKey; the handler derives the key from the
// authenticated principal, never from the request.
type LabsFluidUIService interface {
	Get(user string) (fluidui.State, error)
	Put(user string, doc fluidui.Customization) (fluidui.State, error)
	Reset(user string) (fluidui.State, error)
	Undo(user string) (fluidui.State, error)
}

// WithLabsFluidUI enables the Labs Fluid UI customization routes.
func WithLabsFluidUI(service LabsFluidUIService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API labs fluid UI service is required")
		}
		config.labsFluidUI = service
		return nil
	}
}

// labsFluidUIPath reports the per-user Labs Fluid UI routes (customization
// and requests). Writes to them act only on the caller's own records, so the
// authorizer admits them for the view role instead of operate. It matches
// both concrete request paths and route templates.
func labsFluidUIPath(path string) bool {
	return strings.HasPrefix(path, apicontract.LabsFluidUIPrefix) && !strings.Contains(path, "..") && !strings.Contains(path, "//")
}

func registerLabsFluidUIRoutes(router *Router, service LabsFluidUIService, errorLog *log.Logger) {
	router.HandleByMethod(
		map[string]apicontract.RouteID{
			http.MethodGet:    apicontract.RouteLabsFluidUIGet,
			http.MethodPut:    apicontract.RouteLabsFluidUIPut,
			http.MethodDelete: apicontract.RouteLabsFluidUIReset,
		},
		map[apicontract.RouteID]http.HandlerFunc{
			apicontract.RouteLabsFluidUIGet:   labsFluidUIHandler(service, errorLog, false, labsFluidUIGet),
			apicontract.RouteLabsFluidUIPut:   labsFluidUIHandler(service, errorLog, true, labsFluidUIPut),
			apicontract.RouteLabsFluidUIReset: labsFluidUIHandler(service, errorLog, true, labsFluidUIEmptyBody(LabsFluidUIService.Reset)),
		},
	)
	router.Handle(apicontract.RouteLabsFluidUIUndo, labsFluidUIHandler(service, errorLog, true, labsFluidUIEmptyBody(LabsFluidUIService.Undo)))
}

type labsFluidUIOperation func(LabsFluidUIService, string, *http.Request) (fluidui.State, error)

func labsFluidUIHandler(service LabsFluidUIService, errorLog *log.Logger, write bool, operation labsFluidUIOperation) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "labs_unavailable", "Labs Fluid UI is not configured on this daemon")
			return
		}
		if write {
			if status, code, message := validateMutationTransport(request); status != 0 {
				writeError(w, status, code, message)
				return
			}
		}
		state, err := operation(service, labsFluidUIUser(request), request)
		if err != nil {
			writeLabsFluidUIError(w, errorLog, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	}
}

// labsFluidUIUser keys storage by the authenticated principal. Anonymous
// requests only reach the API under loopback local trust, where the single
// local operator owns the one "local" customization.
func labsFluidUIUser(request *http.Request) string {
	principal, ok := PrincipalFromRequest(request)
	if !ok {
		return fluidui.LocalUser
	}
	return fluidui.UserKey(principal.Issuer, principal.Subject)
}

func labsFluidUIGet(service LabsFluidUIService, user string, _ *http.Request) (fluidui.State, error) {
	return service.Get(user)
}

func labsFluidUIPut(service LabsFluidUIService, user string, request *http.Request) (fluidui.State, error) {
	data, err := io.ReadAll(io.LimitReader(request.Body, fluidui.MaxDocumentBytes+1))
	if err != nil {
		return fluidui.State{}, &fluidui.ValidationError{Field: "$", Message: "request body could not be read"}
	}
	doc, err := fluidui.Decode(data)
	if err != nil {
		return fluidui.State{}, err
	}
	return service.Put(user, doc)
}

// labsFluidUIEmptyBody admits an absent body or an empty JSON object, so
// clients can send {} with the JSON content type the transport check needs.
func labsFluidUIEmptyBody(operation func(LabsFluidUIService, string) (fluidui.State, error)) labsFluidUIOperation {
	return func(service LabsFluidUIService, user string, request *http.Request) (fluidui.State, error) {
		data, err := io.ReadAll(io.LimitReader(request.Body, 64))
		if err != nil {
			return fluidui.State{}, &fluidui.ValidationError{Field: "$", Message: "request body could not be read"}
		}
		if body := strings.TrimSpace(string(data)); body != "" && body != "{}" {
			return fluidui.State{}, &fluidui.ValidationError{Field: "$", Message: "request body must be empty or {}"}
		}
		return operation(service, user)
	}
}

func writeLabsFluidUIError(w http.ResponseWriter, errorLog *log.Logger, err error) {
	var validation *fluidui.ValidationError
	switch {
	case errors.As(err, &validation):
		// The message leads with the field path, e.g. "savedViews[0].route: ...".
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, validation.Error())
	case errors.Is(err, fluidui.ErrNothingToUndo):
		writeError(w, http.StatusConflict, "nothing_to_undo", "there is no earlier customization to restore")
	default:
		errorLog.Printf("labs fluid UI customization failed: %v", err)
		writeError(w, http.StatusInternalServerError, "write_error", "customization could not be saved or read")
	}
}
