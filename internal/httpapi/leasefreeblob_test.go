package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// WithLeaseFreeBlobPlane exempts exactly the blob plane's GET and PUT from a
// closed recovery gate (a cloud control standby, #8188); every other route,
// and the blob plane without the option, stays gated.
func TestLeaseFreeBlobPlaneIsExemptFromTheRecoveryGateAndNothingElseIs(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	blobPath := strings.Replace(apicontract.BlobDigestPath, "{digest}", digest, 1)
	recovering := func(t *testing.T, handler http.Handler, method, path string) bool {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, bytes.NewReader([]byte("x"))))
		var envelope ErrorEnvelope
		_ = json.Unmarshal(response.Body.Bytes(), &envelope)
		return response.Code == http.StatusServiceUnavailable && envelope.Error.Code == CodeRecovering
	}
	build := func(t *testing.T, opts ...HandlerOption) http.Handler {
		t.Helper()
		handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(),
			append([]HandlerOption{WithRecoveryGate(func() bool { return false }), WithBlobService(store)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	exempt := build(t, WithLeaseFreeBlobPlane())
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if recovering(t, exempt, method, blobPath) {
			t.Fatalf("%s blob on a standby was gated, want served", method)
		}
	}
	for _, path := range []string{InstancePath, RunsPath, apicontract.ConfigDigestPath} {
		if !recovering(t, exempt, http.MethodGet, path) {
			t.Fatalf("GET %s on a standby was served, want only the blob plane exempt", path)
		}
	}
	gated := build(t)
	if !recovering(t, gated, http.MethodGet, blobPath) {
		t.Fatal("the blob plane was exempt without WithLeaseFreeBlobPlane")
	}
}
