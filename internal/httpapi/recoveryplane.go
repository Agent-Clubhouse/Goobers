package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
)

// RecoveryService delivers retained state to the authenticated receiving run.
// Implementations must verify its current issue lease and repository identity,
// select authorized retained state, and verify bytes before starting the stream.
// The stream must be bounded and its lifetime capped by lease expiry. This is
// not an arbitrary source-run or content-digest lookup.
type RecoveryService interface {
	StreamRecovery(context.Context, string, string, string, io.Writer) error
}

// WithRecoveryService enables claim-authorized recovery archive delivery.
func WithRecoveryService(service RecoveryService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API recovery service is required")
		}
		config.recovery = service
		return nil
	}
}

// Recovery is a claim-authorized operation, separate from the three own-run
// journal reads. Journal-only or blob-only scoped bearers cannot access it.
func recoveryPlanePath(path string) bool {
	rest, ok := strings.CutPrefix(path, apicontract.RunsPath+"/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) == 2 && apiv1.ValidRunID(parts[0]) && parts[1] == "recovery"
}

func recoveryArchiveHandler(service RecoveryService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		run := request.PathValue("run")
		if !podRunContained(w, request, run, "recovery") {
			return
		}
		key, issue := request.URL.Query().Get("repositoryKey"), request.URL.Query().Get("issue")
		if !apiv1.ValidRunID(run) || key == "" || len(key) > 4096 || issue == "" || len(issue) > 256 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "recovery requires bounded run, repository, and issue identities")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "recovery_unavailable", "recovery delivery is unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		stream := &recoveryResponseWriter{destination: w}
		if err := service.StreamRecovery(request.Context(), run, key, issue, stream); err != nil {
			errorLog.Printf("recovery delivery failed for run %s", run)
			if stream.started {
				// Never append a JSON error to binary archive bytes or report a
				// normally completed response after partial delivery.
				panic(http.ErrAbortHandler)
			}
			writeError(w, http.StatusForbidden, "recovery_refused", "recovery delivery was refused or its state is unavailable")
		}
	}
}

type recoveryResponseWriter struct {
	destination io.Writer
	started     bool
}

func (w *recoveryResponseWriter) Write(data []byte) (int, error) {
	if len(data) != 0 {
		w.started = true
	}
	return w.destination.Write(data)
}
