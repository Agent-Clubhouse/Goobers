package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// WithLeaseFreeBlobPlane changes the closed-gate answer of exactly the two
// blob routes: every other contract route answers on an exempt standby
// exactly as it does on a gated one (#8194 review, reviewer-2's probe).
func TestLeaseFreeOptionChangesOnlyTheBlobRoutes(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	build := func(opts ...HandlerOption) http.Handler {
		h, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(),
			append([]HandlerOption{WithRecoveryGate(func() bool { return false }), WithBlobService(store)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	exempt, gated := build(WithLeaseFreeBlobPlane()), build()
	param := regexp.MustCompile(`\{[^}]+\}`)
	answer := func(h http.Handler, method, path string) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader([]byte("x"))))
		var envelope ErrorEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &envelope)
		return w.Code, envelope.Error.Code
	}
	checked := 0
	for _, route := range apicontract.V1Routes() {
		path := param.ReplaceAllStringFunc(route.Path, func(p string) string {
			if p == "{digest}" {
				return "sha256:" + strings.Repeat("a", 64)
			}
			return "x"
		})
		path = strings.ReplaceAll(path, "{...}", "")
		exemptStatus, exemptCode := answer(exempt, route.Method, path)
		gatedStatus, gatedCode := answer(gated, route.Method, path)
		if route.ID == apicontract.RouteBlobGet || route.ID == apicontract.RouteBlobPut {
			if exemptCode == CodeRecovering {
				t.Errorf("%s %s: blob route gated on a standby", route.Method, route.Path)
			}
			continue
		}
		checked++
		if exemptStatus != gatedStatus || exemptCode != gatedCode {
			t.Errorf("%s %s (%s): exempt answered %d/%s, gated %d/%s; the option widened beyond the blob plane",
				route.Method, route.Path, route.ID, exemptStatus, exemptCode, gatedStatus, gatedCode)
		}
	}
	if checked < 20 {
		t.Fatalf("precondition: only %d non-blob routes compared", checked)
	}
}

// A human operator, however privileged, cannot write the blob plane: only
// stage pods and blob-scoped workers may (#8194 review).
func TestBlobPlaneRefusesAHumanOperator(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	human := &fakeAuthenticator{principal: &Principal{Subject: "alice", Roles: []Role{RoleView, RoleOperate, RoleAdmin}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(human), WithBlobService(store))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("an operator's bytes")
	digest := blobDigest(payload)
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		w := httptest.NewRecorder()
		request := httptest.NewRequest(method, strings.Replace(apicontract.BlobDigestPath, "{digest}", digest, 1), bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/octet-stream")
		handler.ServeHTTP(w, request)
		if w.Code != http.StatusForbidden {
			t.Fatalf("operator %s blob = %d %s, want 403", method, w.Code, w.Body.String())
		}
	}
	if has, err := store.Has(t.Context(), digest); err != nil || has {
		t.Fatalf("an operator's PUT reached the store: present %v (%v)", has, err)
	}
}

func blobDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
