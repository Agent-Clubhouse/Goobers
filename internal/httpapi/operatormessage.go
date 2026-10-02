package httpapi

import (
	"log"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
)

// OperatorMessageSubmissionRequest is the stamped HTTP submission request.
type OperatorMessageSubmissionRequest struct {
	apicontract.OperatorMessageSubmitRequest
	Principal Principal `json:"-"`
}

// OperatorMessageSubmissionResponse is returned after operator-message submission.
type OperatorMessageSubmissionResponse = apicontract.OperatorMessageSubmitResponse

func operatorMessageSubmitHandler(service OperatorMessageService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "operator_messages_unavailable", "operator message submission is not available from this server")
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
		principal, ok := PrincipalFromRequest(request)
		if !ok || strings.TrimSpace(principal.Subject) == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "operator message submission requires an authenticated principal")
			return
		}
		run := request.PathValue("run")
		if !apiv1.ValidRunID(run) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "run id is not a safe path segment")
			return
		}
		var input OperatorMessageSubmissionRequest
		if err := decodeWriteRequestBounded(request, &input.OperatorMessageSubmitRequest, apiv1.MaxOperatorMessageContentBytes); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		input.RunID = run
		input.IdempotencyKey = key
		input.Principal = principal
		input.PrincipalRef = principalRef(principal)
		response, err := service.SubmitOperatorMessage(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "submit operator message", err)
			return
		}
		status := http.StatusOK
		if response.Accepted {
			status = http.StatusAccepted
		}
		writeJSON(w, status, response)
	}
}

func principalRef(principal Principal) string {
	subject := strings.TrimSpace(principal.Subject)
	issuer := strings.TrimSpace(principal.Issuer)
	if issuer == "" {
		return subject
	}
	return issuer + ":" + subject
}
