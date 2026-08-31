package httpapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// blobplane.go implements the daemon write API's blob plane (decision
// 010/012, §2a): the daemon-fronted GET/PUT-by-digest routes a mode-3 stage
// pod's BlobClient (internal/dispatcher/blob.go, BlobPathPrefix) addresses to
// fetch and put content-addressed artifacts over the network in place of a
// shared filesystem or a dispatcher-brokered byte path. The daemon is not a
// second store: it fronts the SAME blobstore.Store a local worker plugs into
// workerhost.MaterializeContext / StagingArtifacts — this plane is the
// network transport for that one seam, never a parallel implementation of it.
//
// CONTENT-ADDRESSED, SO THE DIGEST IS THE INTEGRITY CHECK:
//   - GET answers 200 with the stored bytes, or 404 — a blob is present or it
//     is not, and content-addressing means there is nothing else a read could
//     disagree about.
//   - PUT verifies the request body actually hashes to the digest named in the
//     URL BEFORE it ever reaches the store. A body that does not hash to its
//     claimed digest is a client bug or an attack, refused with 400 rather
//     than silently filed under the wrong address.
//   - PUT is idempotent by construction: blobstore.Store.Put's own contract
//     treats a digest already present as a no-op, so a retried PUT of
//     identical (digest-verified, so by definition identical) content is a
//     204, never a conflict.
//
// AUTHENTICATION mirrors the credential plane (DS9), not the claims plane:
// raw content-addressed bytes carry no run scope to compare a pod's token
// against, so containment cannot be "this run's own claims/journal/credential
// resolve" — it is "an authenticated stage pod, or refused" full stop. A
// human OIDC principal, however privileged, is refused: mode 1/2 resolve
// blobs in-process (StagingArtifacts talks to blobstore.Store directly) and
// never need this transport, so the loopback null-auth posture's convenience
// does not extend to it either.

// MaxBlobBytes bounds one blob transfer. blobstore.Store.Put takes the whole
// blob as []byte — there is no streaming variant — so the body is fully
// buffered before its digest can even be checked; this keeps that buffering
// bounded rather than unlimited. Generous relative to the journal plane's 4
// MiB maxJournalEmitBody (an incidental payload riding a journal batch)
// because this route IS the dedicated artifact-transfer plane.
const MaxBlobBytes = 64 << 20 // 64 MiB

// WithBlobService enables the blob plane's digest GET/PUT routes. store is
// the SAME blobstore.Store type a local worker plugs into
// MaterializeContext/StagingArtifacts — the daemon fronts it over HTTP rather
// than holding a second implementation of the content-addressed store.
func WithBlobService(store blobstore.Store) HandlerOption {
	return func(config *handlerConfig) error {
		if store == nil {
			return errors.New("http API blob store is required")
		}
		config.blobs = store
		return nil
	}
}

// registerBlobPlaneRoutes registers GET and PUT on the shared digest path
// (HandleByMethod, since net/http's ServeMux rejects two bare registrations
// of the same pattern). Registered unconditionally, like the claims and
// trigger planes: a nil store still answers a structured 503 rather than the
// routes silently not existing, and a mode-1/2 daemon that is never asked to
// serve a mode-3 stage never receives a request on them at all.
func registerBlobPlaneRoutes(router *Router, store blobstore.Store, errorLog *log.Logger) {
	router.HandleByMethod(
		map[string]apicontract.RouteID{
			http.MethodGet: apicontract.RouteBlobGet,
			http.MethodPut: apicontract.RouteBlobPut,
		},
		map[apicontract.RouteID]http.HandlerFunc{
			apicontract.RouteBlobGet: blobGetHandler(store, errorLog),
			apicontract.RouteBlobPut: blobPutHandler(store, errorLog),
		},
	)
}

func blobGetHandler(store blobstore.Store, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if store == nil {
			writeError(w, http.StatusServiceUnavailable, "blobs_unavailable", "the blob plane is not available from this server")
			return
		}
		if !requireBlobPodPrincipal(w, request) {
			return
		}
		digest := request.PathValue("digest")
		if !blobstore.ValidDigest(digest) {
			writeError(w, http.StatusBadRequest, "invalid_digest", "digest is not a valid sha256:<hex> content address")
			return
		}
		data, err := store.Get(request.Context(), digest)
		if err != nil {
			if errors.Is(err, blobstore.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "blob not found")
				return
			}
			errorLog.Printf("get blob %s failed: %v", digest, err)
			writeError(w, http.StatusInternalServerError, "blob_read_failed", "blob could not be read")
			return
		}
		// Content is content-addressed and immutable: once a digest resolves,
		// its bytes never change, so the response can be cached forever.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"`+digest+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func blobPutHandler(store blobstore.Store, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if store == nil {
			writeError(w, http.StatusServiceUnavailable, "blobs_unavailable", "the blob plane is not available from this server")
			return
		}
		if !requireBlobPodPrincipal(w, request) {
			return
		}
		digest := request.PathValue("digest")
		if !blobstore.ValidDigest(digest) {
			writeError(w, http.StatusBadRequest, "invalid_digest", "digest is not a valid sha256:<hex> content address")
			return
		}
		defer func() { _ = request.Body.Close() }()
		data, err := io.ReadAll(http.MaxBytesReader(w, request.Body, MaxBlobBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "blob_too_large",
					fmt.Sprintf("blob body exceeds %d bytes", MaxBlobBytes))
				return
			}
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "blob body could not be read")
			return
		}
		// The whole point of content-addressing: a PUT whose body does not hash
		// to its claimed digest is a client bug or an attack, refused before it
		// ever reaches the store rather than filed under the wrong address.
		if actual := fmt.Sprintf("sha256:%x", sha256.Sum256(data)); actual != digest {
			writeError(w, http.StatusBadRequest, "digest_mismatch",
				fmt.Sprintf("body hashes to %s, not the claimed %s", actual, digest))
			return
		}
		if err := store.Put(request.Context(), digest, data); err != nil {
			errorLog.Printf("put blob %s failed: %v", digest, err)
			writeError(w, http.StatusInternalServerError, "blob_write_failed", "blob could not be stored")
			return
		}
		// blobstore.Store.Put is idempotent by content address: a digest already
		// present is a no-op there, and a success here either way — never a
		// conflict, which is what lets the plane be shared fleet-wide with no
		// locking (blobstore package doc).
		w.WriteHeader(http.StatusNoContent)
	}
}

// requireBlobPodPrincipal enforces the blob plane's fail-closed posture,
// mirroring the credential plane (DS9): it requires an authenticated POD
// principal UNCONDITIONALLY. Unlike the claims/journal planes there is no
// run id in the request to additionally bind the principal to — a digest
// carries no run scope — so "pod principal, full stop" is the entire
// containment. A human OIDC principal, however privileged, is refused, and an
// unauthenticated request under the loopback null-auth posture is refused
// too: mode 1/2 never call this transport at all.
func requireBlobPodPrincipal(w http.ResponseWriter, request *http.Request) bool {
	principal, authenticated := PrincipalFromRequest(request)
	if !authenticated || !IsPodPrincipal(principal) {
		writeError(w, http.StatusForbidden, "blob_plane_requires_pod_principal",
			"the blob plane requires an authenticated pod principal; it serves stage pods only")
		return false
	}
	return true
}
