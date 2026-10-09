package httpapi

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

func TestGeneratedChildWithoutArtifactOwnerCannotUseSharedStore(t *testing.T) {
	base, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("private shared artifact")
	digest := journal.Digest(data)
	if err := base.Put(t.Context(), digest, data); err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			r := httptest.NewRequest(method, "/api/v1/blobs/"+digest, bytes.NewReader(data))
			r.SetPathValue("digest", digest)
			principal := Principal{Subject: "run:" + strings.Repeat("1", 32), Issuer: GeneratedChildPrincipalIssuer, GeneratedChild: &GeneratedChildPrincipal{ContractDigest: journal.Digest([]byte("contract"))}}
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
			ordinary, child := blobGetHandler(base, logger), blobGetScopedHandler(nil, logger, true)
			if method == http.MethodPut {
				ordinary, child = blobPutHandler(base, logger), blobPutScopedHandler(nil, logger, true)
			}
			w := httptest.NewRecorder()
			childBlobRoute(ordinary, child, nil)(w, r)
			if w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("missing owner fell through", w.Code, w.Body, w.Header())
			}
			// Even invoking the ordinary handler directly cannot reinterpret this issuer.
			w = httptest.NewRecorder()
			ordinary(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatal("ordinary handler admitted child", w.Code)
			}
		})
	}
}

func TestGeneratedChildIdentityIsConfinedToInstalledAttemptOwners(t *testing.T) {
	paths := []string{apicontract.ConfigDigestPath, apicontract.WorkerConfigDivergencePath, RunsPath, EventsPath, HealthPath,
		"/api/v1/runs/run-1/operator-messages", apicontract.ConfigDigestPath + "/", "/api/v1/unknown"}
	for _, route := range apicontract.V1Routes() {
		paths = append(paths, route.Path)
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			request := httptest.NewRequest(method, path, nil)
			// Even accidentally supplied roles/scopes cannot widen this identity.
			principal := Principal{Subject: "run:" + strings.Repeat("1", 32), Issuer: GeneratedChildPrincipalIssuer, GeneratedChild: &GeneratedChildPrincipal{ContractDigest: journal.Digest([]byte("contract"))}, Roles: []Role{RoleAdmin}, Scopes: knownPodScopes()}
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
			err := RequireRoles().Authorize(request)
			want := (method == http.MethodGet || method == http.MethodPut) && blobPlanePath(path)
			want = want || (method == http.MethodPost && path == apicontract.CredentialResolvePath)
			// The read-only execution owner rejects non-execution and foreign
			// requests; generatedchildexecution_test covers that handler boundary.
			want = want || (method == http.MethodPost && path == apicontract.ClaimListPath)
			want = want || (method == http.MethodPost && journalPlanePath(path))
			want = want || (method == http.MethodPost && surrenderPlanePath(path))
			if (err == nil) != want {
				t.Fatalf("worker admitted %s %s=%v, want %v", method, path, err == nil, want)
			}
		}
	}
}
