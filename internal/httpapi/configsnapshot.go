package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
)

// ConfigSnapshotService serves sanitized worker configuration by digest.
type ConfigSnapshotService interface {
	Current() apicontract.ConfigSnapshotCurrent
	Open(string) (io.ReadCloser, int64, error)
}

// WithConfigSnapshotService enables the worker-only configuration plane.
func WithConfigSnapshotService(service ConfigSnapshotService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API config snapshot service is required")
		}
		config.configSnapshots = service
		return nil
	}
}

func registerConfigSnapshotRoute(router *Router, service ConfigSnapshotService) {
	router.Handle(apicontract.RouteConfigSnapshot, func(w http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFromRequest(request)
		if !ok || principal.Issuer != WorkerPrincipalIssuer {
			writeError(w, http.StatusForbidden, "worker_identity_required", "only an authenticated worker may fetch configuration")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "config_snapshot_unavailable", "worker configuration is unavailable")
			return
		}
		digest := strings.TrimSpace(request.URL.Query().Get("digest"))
		if digest == "" {
			current := service.Current()
			if current.Digest == "" || current.InstanceID == "" {
				writeError(w, http.StatusServiceUnavailable, "config_snapshot_unavailable", "worker configuration is not published yet")
				return
			}
			writeJSON(w, http.StatusOK, current)
			return
		}
		file, size, err := service.Open(digest)
		if err != nil {
			writeError(w, http.StatusNotFound, "config_snapshot_not_found", "worker configuration snapshot was not found")
			return
		}
		defer func() { _ = file.Close() }()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set(apicontract.DigestHeader, digest)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, &contextReader{ctx: request.Context(), reader: file})
	})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
