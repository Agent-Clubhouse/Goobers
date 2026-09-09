package httpapi

import (
	"context"
	"io"
	"log"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// RecoveryPublisher authenticates the current issue claim before reading the
// request, verifies the archive, and returns only after durable host custody.
// It must revalidate authorization before acknowledgement. This is deliberately
// separate from download support: a read-only service cannot accept uploads.
type RecoveryPublisher interface {
	PublishRecovery(context.Context, string, string, string, io.Reader) error
}

const maxRecoveryUploadBytes int64 = (512 << 20) + (16 << 10) + 4

func recoveryPublishHandler(service RecoveryService, errorLog *log.Logger) http.HandlerFunc {
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
		publisher, ok := service.(RecoveryPublisher)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "recovery_unavailable", "recovery publication is unavailable")
			return
		}
		if request.ContentLength > maxRecoveryUploadBytes {
			writeError(w, http.StatusRequestEntityTooLarge, CodeInvalidRequest, "recovery archive exceeds byte limit")
			return
		}
		body := http.MaxBytesReader(w, request.Body, maxRecoveryUploadBytes)
		defer func() { _ = body.Close() }()
		if err := publisher.PublishRecovery(request.Context(), run, key, issue, body); err != nil {
			errorLog.Printf("recovery publication failed for run %s", run)
			writeError(w, http.StatusForbidden, "recovery_refused", "recovery publication was refused or custody is unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
