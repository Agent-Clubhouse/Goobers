package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// surrenderplane.go is the write API's surrender plane (#3699): a mode-3
// stage pod's dispatch-exec entrypoint PUTs its SurrenderedResult here
// before exiting, so the engine's dispatch activity can read it back by
// attempt identity (dispatcher.ReadSurrenderedResult). It is the structural
// sibling of the journal plane (journalplane.go) — an identity-keyed pod
// write, authenticated as the pod's own run, never a shared PVC (the same
// network-plane pattern the blob plane already ships for artifacts,
// decision 010) — not of the blob plane, which is content-addressed and
// cannot serve an identity-keyed lookup (dispatcher.SurrenderPlane's own doc
// comment explains why).

// maxSurrenderBody bounds one surrendered result document. v1's dispatch-exec
// inlines bounded stdout/stderr into the ResultEnvelope rather than
// journaling artifacts from in-pod, so this stays well under the journal
// plane's cap.
const maxSurrenderBody = 1 << 20

// SurrenderService accepts one attempt's surrendered result document. The
// shipped implementation is *dispatcher.SurrenderDir (via SurrenderPlane's
// identical Put signature); this package declares its own narrow interface —
// the same shape JournalService uses for livejournal.Writer — rather than
// importing internal/dispatcher, whose own tests already import httpapi
// (that direction would cycle).
type SurrenderService interface {
	Put(ctx context.Context, runID, stage string, attempt int, data []byte) error
}

// WithSurrenderService enables surrender writes and, when the backend also
// implements SurrenderReader, worker-only bounded reads. Only wired when
// the daemon is configured for mode-3 dispatch (cmd/goobers/workerdispatch.go)
// — a non-cloud instance never registers this option, so the route never
// exists for it.
func WithSurrenderService(plane SurrenderService) HandlerOption {
	return func(config *handlerConfig) error {
		if plane == nil {
			return errors.New("http API surrender plane is required")
		}
		config.surrenders = plane
		return nil
	}
}

// WithGeneratedChildSurrenderService installs the exact-attempt result owner.
// This never grants generated children the worker-only surrender read routes.
func WithGeneratedChildSurrenderService(plane SurrenderService) HandlerOption {
	return func(config *handlerConfig) error {
		if plane == nil {
			return errors.New("generated child surrender owner is required")
		}
		config.generatedChildSurrenders = plane
		return nil
	}
}

// surrenderedResultShape is the minimal shape this route validates before
// storing the request body verbatim: it needs to know the document carries a
// real terminal status, not the full dispatcher.SurrenderedResult schema
// (which would require importing internal/dispatcher — see SurrenderService).
type surrenderedResultShape struct {
	Result struct {
		Status string `json:"status"`
	} `json:"result"`
}

func registerSurrenderPlaneRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	plane := config.surrenders
	router.Handle(apicontract.RouteStageSurrenderSeen, surrenderReadHandler(plane, true, errorLog))
	router.HandleByMethod(map[string]apicontract.RouteID{
		http.MethodPost: apicontract.RouteStageSurrender, http.MethodGet: apicontract.RouteStageSurrenderGet,
	}, map[apicontract.RouteID]http.HandlerFunc{
		apicontract.RouteStageSurrenderGet: surrenderReadHandler(plane, false, errorLog),
		apicontract.RouteStageSurrender: func(w http.ResponseWriter, request *http.Request) {
			plane := config.surrenders
			principal, authenticated := PrincipalFromRequest(request)
			generated := authenticated && principal.Issuer == GeneratedChildPrincipalIssuer
			if generated {
				w.Header().Set("Cache-Control", "private, no-store")
				if config.generatedChildSurrenders == nil || principal.GeneratedChild == nil || !blobstore.ValidDigest(principal.GeneratedChild.ContractDigest) {
					writeError(w, http.StatusForbidden, "child_surrender_unavailable", "generated child result authority is not available")
					return
				}
				plane = config.generatedChildSurrenders
			}
			isParent := authenticated && principal.Issuer == WorkflowParentPrincipalIssuer
			if isParent {
				w.Header().Set("Cache-Control", "private, no-store")
				if config.workflowParentSurrenders == nil || principal.WorkflowParent == nil || !blobstore.ValidDigest(principal.WorkflowParent.ContractDigest) {
					writeError(w, http.StatusForbidden, "parent_surrender_unavailable", "contained parent result authority is not available")
					return
				}
				plane = config.workflowParentSurrenders
			}
			if plane == nil {
				writeError(w, http.StatusServiceUnavailable, "surrender_unavailable", "the surrender plane is not available from this server")
				return
			}
			if status, code, message := validateMutationTransport(request); status != 0 {
				writeError(w, status, code, message)
				return
			}
			run := request.PathValue("run")
			if !apiv1.ValidRunID(run) {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "run id is not a safe path segment")
				return
			}
			stage := request.PathValue("stage")
			if !apiv1.ValidRunID(stage) {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "stage name is not a safe path segment")
				return
			}
			attempt, err := strconv.Atoi(request.PathValue("attempt"))
			if err != nil || attempt < 1 {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "attempt must be a positive integer")
				return
			}
			// Per-run containment: a pod token proves "I am run X's stage pod",
			// which authorizes surrendering a result for run X and no other —
			// the same body-level binding the journal plane applies.
			if authenticated && (IsPodPrincipal(principal) || generated || isParent) {
				if principal.Subject != podPrincipalSubject(run) {
					writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only surrender its own run's results")
					return
				}
			}
			body, status, message := readSurrenderedResult(request)
			if status != 0 {
				writeError(w, status, CodeInvalidRequest, message)
				return
			}
			if err := plane.Put(request.Context(), run, stage, attempt, body); err != nil {
				writePlaneError(w, errorLog, "put surrendered result", err)
				return
			}
			writeJSON(w, http.StatusOK, struct{}{})
		}})
}

// WithWorkflowParentSurrenderService installs write-only parent result custody.
func WithWorkflowParentSurrenderService(plane SurrenderService) HandlerOption {
	return func(config *handlerConfig) error {
		if plane == nil {
			return errors.New("contained parent surrender owner is required")
		}
		config.workflowParentSurrenders = plane
		return nil
	}
}

func readSurrenderedResult(request *http.Request) ([]byte, int, string) {
	defer func() { _ = request.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(request.Body, maxSurrenderBody+1))
	if err != nil {
		return nil, http.StatusBadRequest, "request body could not be read"
	}
	if int64(len(body)) > maxSurrenderBody {
		return nil, http.StatusRequestEntityTooLarge, "surrendered result body exceeds the size limit"
	}
	var shape surrenderedResultShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return nil, http.StatusBadRequest, "invalid JSON request body"
	}
	if shape.Result.Status == "" {
		return nil, http.StatusBadRequest, "surrendered result carries no status"
	}
	return body, 0, ""
}
